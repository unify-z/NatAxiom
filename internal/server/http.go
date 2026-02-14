package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/unify-z/NatAxiom/internal/config"
	"github.com/unify-z/NatAxiom/internal/proto"
	"github.com/unify-z/NatAxiom/internal/util"
)

type HTTPProxy struct {
	cfg      config.ServerConfig
	logger   *slog.Logger
	sessions *sync.Map

	mu      sync.RWMutex
	domains map[string]domainRoute
	certs   map[string]*tls.Certificate
}

type domainRoute struct {
	sessionID  string
	tunnelName string
}

func NewHTTPProxy(cfg config.ServerConfig, logger *slog.Logger, sessions *sync.Map) *HTTPProxy {
	return &HTTPProxy{
		cfg:      cfg,
		logger:   logger,
		sessions: sessions,
		domains:  make(map[string]domainRoute),
		certs:    make(map[string]*tls.Certificate),
	}
}

func (p *HTTPProxy) RegisterDomain(domain, sessionID, tunnelName string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.domains[domain]; exists {
		return fmt.Errorf("domain %s already registered", domain)
	}
	p.domains[domain] = domainRoute{sessionID: sessionID, tunnelName: tunnelName}
	p.logger.Info("domain registered", "domain", domain, "tunnel", tunnelName)
	return nil
}

func (p *HTTPProxy) RegisterCertificate(domain, certPEM, keyPEM string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return fmt.Errorf("invalid certificate for domain %s: %w", domain, err)
	}

	if p.certs == nil {
		p.certs = make(map[string]*tls.Certificate)
	}
	p.certs[domain] = &cert
	p.logger.Info("certificate registered", "domain", domain)
	return nil
}

func (p *HTTPProxy) UnregisterDomain(domain string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.domains, domain)
	delete(p.certs, domain)
	p.logger.Info("domain unregistered", "domain", domain)
}

func (p *HTTPProxy) UnregisterSession(sessionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for domain, route := range p.domains {
		if route.sessionID == sessionID {
			delete(p.domains, domain)
			delete(p.certs, domain)
			p.logger.Info("domain unregistered", "domain", domain, "session", sessionID)
		}
	}
}

func (p *HTTPProxy) ServeHTTP(ctx context.Context) error {
	if p.cfg.HTTP.Port == 0 {
		return nil
	}
	addr := fmt.Sprintf("%s:%d", p.cfg.HTTP.BindAddr, p.cfg.HTTP.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen http: %w", err)
	}
	defer ln.Close()

	p.logger.Info("http proxy listening", "addr", addr)

	server := &http.Server{
		Handler:      http.HandlerFunc(p.handleHTTPRequest),
		ReadTimeout:  p.cfg.Server.ReadTimeout(),
		WriteTimeout: p.cfg.Server.WriteTimeout(),
	}

	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()

	if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (p *HTTPProxy) ServeHTTPS(ctx context.Context) error {
	if p.cfg.HTTPS.Port == 0 {
		return nil
	}
	addr := fmt.Sprintf("%s:%d", p.cfg.HTTPS.BindAddr, p.cfg.HTTPS.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen https: %w", err)
	}
	defer ln.Close()

	tlsConfig, err := p.buildTLSConfig()
	if err != nil {
		return fmt.Errorf("build tls config: %w", err)
	}

	p.logger.Info("https proxy listening", "addr", addr)

	server := &http.Server{
		Handler:      http.HandlerFunc(p.handleHTTPRequest),
		TLSConfig:    tlsConfig,
		ReadTimeout:  p.cfg.Server.ReadTimeout(),
		WriteTimeout: p.cfg.Server.WriteTimeout(),
	}

	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()

	if err := server.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (p *HTTPProxy) buildTLSConfig() (*tls.Config, error) {
	return &tls.Config{
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			p.mu.RLock()
			defer p.mu.RUnlock()

			if cert, ok := p.certs[hello.ServerName]; ok {
				return cert, nil
			}
			return nil, fmt.Errorf("no certificate for domain: %s", hello.ServerName)
		},
	}, nil
}

func (p *HTTPProxy) handleHTTPRequest(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if idx := strings.Index(host, ":"); idx != -1 {
		host = host[:idx]
	}

	p.mu.RLock()
	route, ok := p.domains[host]
	p.mu.RUnlock()

	if !ok {
		p.logger.Warn("domain not found", "host", host, "path", r.URL.Path)
		http.Error(w, "Domain not found", http.StatusNotFound)
		return
	}

	value, ok := p.sessions.Load(route.sessionID)
	if !ok {
		p.logger.Warn("session not found", "session", route.sessionID, "domain", host)
		http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}

	sess := value.(*session)
	connID := util.NewID()
	if connID == "" {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		p.logger.Warn("hijack failed", "err", err)
		return
	}

	if !sess.addPendingHTTP(connID, route.tunnelName, clientConn, r) {
		_ = clientConn.Close()
		return
	}

	if err := sess.sendOpen(route.tunnelName, connID); err != nil {
		p.logger.Warn("failed to send open", "tunnel", route.tunnelName, "err", err)
		sess.removePending(connID)
		return
	}
}

func (s *session) addPendingHTTP(connID, tunnelName string, conn net.Conn, r *http.Request) bool {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.pendingCounts[tunnelName] >= s.cfg.MaxPendingPerTunnel {
		s.logger.Warn("pending limit reached", "tunnel", tunnelName)
		return false
	}

	s.pending[connID] = pendingConn{
		conn:       conn,
		tunnelName: tunnelName,
		createdAt:  time.Now(),
		httpReq:    r,
	}
	s.pendingCounts[tunnelName]++
	s.logger.Debug("pending http connection added", "tunnel", tunnelName, "conn_id", connID)

	time.AfterFunc(s.cfg.PendingTimeout(), func() {
		if s.removePending(connID) {
			s.logger.Warn("pending connection timed out", "tunnel", tunnelName, "conn_id", connID)
		}
	})
	return true
}

func (s *session) sendOpen(tunnelName, connID string) error {
	_ = s.conn.RawConn().SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout()))
	return s.conn.WriteMessage(proto.Open{
		TunnelName: tunnelName,
		ConnID:     connID,
	})
}
