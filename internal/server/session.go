package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/unify-z/NatAxiom/internal/config"
	"github.com/unify-z/NatAxiom/internal/proto"
	"github.com/unify-z/NatAxiom/internal/util"
)

type session struct {
	id           string
	clientID     string
	cfg          config.ServerSettings
	logger       *slog.Logger
	conn         *proto.Conn
	portRegistry *portRegistry
	httpProxy    *HTTPProxy
	onClose      func()

	mu         sync.Mutex
	registered bool
	tunnels    map[string]proto.TunnelSpec
	listeners  map[int]net.Listener
	udpProxies map[int]*UDPProxy
	closed     chan struct{}
	closeOnce  sync.Once

	pendingMu     sync.Mutex
	pending       map[string]pendingConn
	pendingCounts map[string]int
}

type pendingConn struct {
	conn       net.Conn
	tunnelName string
	createdAt  time.Time
	httpReq    *http.Request
}

func newSession(id, clientID string, cfg config.ServerSettings, logger *slog.Logger, conn *proto.Conn, registry *portRegistry, httpProxy *HTTPProxy, onClose func()) *session {
	return &session{
		id:            id,
		clientID:      clientID,
		cfg:           cfg,
		logger:        logger,
		conn:          conn,
		portRegistry:  registry,
		httpProxy:     httpProxy,
		onClose:       onClose,
		closed:        make(chan struct{}),
		pending:       make(map[string]pendingConn),
		pendingCounts: make(map[string]int),
		udpProxies:    make(map[int]*UDPProxy),
	}
}

func (s *session) Register(tunnels []proto.TunnelSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registered {
		return errors.New("client already registered")
	}
	if len(tunnels) == 0 {
		return errors.New("no tunnels provided")
	}

	seen := map[string]struct{}{}
	for _, t := range tunnels {
		if t.Name == "" {
			return errors.New("tunnel name is required")
		}
		if _, ok := seen[t.Name]; ok {
			return fmt.Errorf("duplicate tunnel name: %s", t.Name)
		}
		seen[t.Name] = struct{}{}

		if t.Type == "" {
			t.Type = "tcp"
		}

		switch t.Type {
		case "tcp", "udp":
			if t.RemotePort <= 0 || t.RemotePort > 65535 {
				return fmt.Errorf("invalid remote port for tunnel %s", t.Name)
			}
		case "http", "https":
			if t.Domain == "" {
				return fmt.Errorf("domain is required for %s tunnel %s", t.Type, t.Name)
			}
		default:
			return fmt.Errorf("unsupported tunnel type %s for tunnel %s", t.Type, t.Name)
		}
	}

	listeners := make(map[int]net.Listener)
	udpProxies := make(map[int]*UDPProxy)
	registered := make(map[string]proto.TunnelSpec)

	cleanup := func() {
		for port, ln := range listeners {
			_ = ln.Close()
			s.portRegistry.Release(port, s)
		}
		for port := range udpProxies {
			s.portRegistry.Release(port, s)
		}
	}

	for _, t := range tunnels {
		switch t.Type {
		case "tcp":
			if err := s.portRegistry.Reserve(t.RemotePort, s); err != nil {
				cleanup()
				return err
			}
			addr := fmt.Sprintf("%s:%d", s.cfg.BindAddr, t.RemotePort)
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				s.portRegistry.Release(t.RemotePort, s)
				cleanup()
				return fmt.Errorf("listen on %s: %w", addr, err)
			}
			listeners[t.RemotePort] = ln
			registered[t.Name] = t
			go s.acceptTunnel(t.Name, ln)

		case "udp":
			if err := s.portRegistry.Reserve(t.RemotePort, s); err != nil {
				cleanup()
				return err
			}
			proxy := NewUDPProxy(s.logger, s.cfg.UdpIdleTimeout())
			udpProxies[t.RemotePort] = proxy
			registered[t.Name] = t
			go func(tunnelName string, port int) {
				if err := proxy.Start(s.cfg.BindAddr, port, func(clientAddr string) (net.Conn, error) {
					return s.createDataConnection(tunnelName)
				}); err != nil {
					s.logger.Warn("udp proxy failed", "tunnel", tunnelName, "err", err)
				}
			}(t.Name, t.RemotePort)

		case "http", "https":
			if s.httpProxy == nil {
				cleanup()
				return fmt.Errorf("http proxy not available for tunnel %s", t.Name)
			}
			if err := s.httpProxy.RegisterDomain(t.Domain, s.id, t.Name); err != nil {
				cleanup()
				return err
			}
			if t.Type == "https" {
				if err := s.httpProxy.RegisterCertificate(t.Domain, t.CertPEM, t.KeyPEM); err != nil {
					cleanup()
					return fmt.Errorf("failed to register certificate for %s: %w", t.Domain, err)
				}
			}
			registered[t.Name] = t
		}
	}

	s.listeners = listeners
	s.udpProxies = udpProxies
	s.tunnels = registered
	s.registered = true
	return nil
}

func (s *session) acceptTunnel(name string, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.isClosed() || errors.Is(err, net.ErrClosed) {
				return
			}
			s.logger.Warn("tunnel accept failed", "tunnel", name, "err", err)
			continue
		}
		util.EnableTCPKeepAlive(conn)
		s.logger.Debug("remote connection accepted", "tunnel", name, "remote", conn.RemoteAddr().String())

		connID := util.NewID()
		if connID == "" {
			_ = conn.Close()
			continue
		}

		if !s.addPending(connID, name, conn) {
			_ = conn.Close()
			continue
		}

		_ = s.conn.RawConn().SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout()))
		err = s.conn.WriteMessage(proto.Open{TunnelName: name, ConnID: connID})
		if err != nil {
			s.logger.Warn("failed to send open", "tunnel", name, "err", err)
			_ = conn.Close()
			s.Close()
			return
		}
		s.logger.Debug("open sent", "tunnel", name, "conn_id", connID)
	}
}

func (s *session) addPending(connID, tunnelName string, conn net.Conn) bool {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.pendingCounts[tunnelName] >= s.cfg.MaxPendingPerTunnel {
		s.logger.Warn("pending limit reached", "tunnel", tunnelName)
		return false
	}
	s.pending[connID] = pendingConn{conn: conn, tunnelName: tunnelName, createdAt: time.Now()}
	s.pendingCounts[tunnelName]++
	s.logger.Debug("pending connection added", "tunnel", tunnelName, "conn_id", connID)

	time.AfterFunc(s.cfg.PendingTimeout(), func() {
		if s.removePending(connID) {
			s.logger.Warn("pending connection timed out", "tunnel", tunnelName, "conn_id", connID)
		}
	})
	return true
}

func (s *session) removePending(connID string) bool {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	p, ok := s.pending[connID]
	if !ok {
		return false
	}
	delete(s.pending, connID)
	if s.pendingCounts[p.tunnelName] > 0 {
		s.pendingCounts[p.tunnelName]--
	}
	_ = p.conn.Close()
	return true
}

func (s *session) AttachData(connID string, dataConn net.Conn) bool {
	s.pendingMu.Lock()
	p, ok := s.pending[connID]
	if ok {
		delete(s.pending, connID)
		if s.pendingCounts[p.tunnelName] > 0 {
			s.pendingCounts[p.tunnelName]--
		}
	}
	s.pendingMu.Unlock()
	if !ok {
		return false
	}
	s.logger.Debug(
		"data channel attached",
		"tunnel", p.tunnelName,
		"conn_id", connID,
		"remote", dataConn.RemoteAddr().String(),
		"pending_ms", time.Since(p.createdAt).Milliseconds(),
	)

	if p.httpReq != nil {
		go func() {
			defer p.conn.Close()
			defer dataConn.Close()

			if err := p.httpReq.Write(dataConn); err != nil {
				s.logger.Warn("failed to write http request to data conn", "err", err, "tunnel", p.tunnelName, "conn_id", connID)
				return
			}

			label := p.tunnelName + ":" + connID
			s.logger.Debug("http proxy start", "label", label)
			util.ProxyWithLogger(p.conn, dataConn, s.logger, label)
			s.logger.Debug("http proxy done", "label", label)
		}()
		return true
	}

	label := p.tunnelName + ":" + connID
	go util.ProxyWithLogger(p.conn, dataConn, s.logger, label)
	return true
}

func (s *session) Close() {
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.onClose != nil {
			s.onClose()
		}
		_ = s.conn.Close()
		for port, ln := range s.listeners {
			_ = ln.Close()
			s.portRegistry.Release(port, s)
		}
		for port := range s.udpProxies {
			s.portRegistry.Release(port, s)
		}
		if s.httpProxy != nil {
			s.httpProxy.UnregisterSession(s.id)
		}
		s.pendingMu.Lock()
		for _, p := range s.pending {
			_ = p.conn.Close()
		}
		s.pending = map[string]pendingConn{}
		s.pendingCounts = map[string]int{}
		s.pendingMu.Unlock()
	})
}

func (s *session) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

type portRegistry struct {
	mu     sync.Mutex
	owners map[int]*session
}

func newPortRegistry() *portRegistry {
	return &portRegistry{owners: make(map[int]*session)}
}

func (r *portRegistry) Reserve(port int, s *session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if owner, ok := r.owners[port]; ok && owner != s {
		return fmt.Errorf("port %d already reserved", port)
	}
	r.owners[port] = s
	return nil
}

func (r *portRegistry) Release(port int, s *session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if owner, ok := r.owners[port]; ok && owner == s {
		delete(r.owners, port)
	}
}

func (s *session) createDataConnection(tunnelName string) (net.Conn, error) {
	connID := util.NewID()
	if connID == "" {
		return nil, errors.New("failed to generate conn id")
	}

	clientConn, serverConn := net.Pipe()

	if !s.addPending(connID, tunnelName, serverConn) {
		_ = clientConn.Close()
		_ = serverConn.Close()
		return nil, errors.New("failed to add pending connection")
	}

	if err := s.sendOpen(tunnelName, connID); err != nil {
		s.removePending(connID)
		_ = clientConn.Close()
		return nil, err
	}

	return clientConn, nil
}
