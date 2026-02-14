package server

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/unify-z/NatAxiom/internal/util"
)

type UDPProxy struct {
	logger       *slog.Logger
	idleTimeout  time.Duration
	mu           sync.RWMutex
	sessions     map[string]*udpSession
	cleanupTimer *time.Timer
}

type udpSession struct {
	clientAddr *net.UDPAddr
	tunnelConn net.Conn
	lastActive time.Time
}

func NewUDPProxy(logger *slog.Logger, idleTimeout time.Duration) *UDPProxy {
	return &UDPProxy{
		logger:      logger,
		idleTimeout: idleTimeout,
		sessions:    make(map[string]*udpSession),
	}
}

func (p *UDPProxy) Start(bindAddr string, port int, onNewSession func(clientAddr string) (net.Conn, error)) error {
	addr := fmt.Sprintf("%s:%d", bindAddr, port)
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return fmt.Errorf("resolve udp addr: %w", err)
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("listen udp: %w", err)
	}

	p.logger.Info("udp proxy listening", "addr", addr)

	go p.cleanupLoop()

	buf := make([]byte, 65536)
	for {
		n, clientAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			p.logger.Warn("udp read error", "err", err)
			continue
		}

		key := clientAddr.String()
		p.mu.RLock()
		sess, exists := p.sessions[key]
		p.mu.RUnlock()

		if !exists {
			tunnelConn, err := onNewSession(key)
			if err != nil {
				p.logger.Warn("failed to create tunnel connection", "client", key, "err", err)
				continue
			}

			sess = &udpSession{
				clientAddr: clientAddr,
				tunnelConn: tunnelConn,
				lastActive: time.Now(),
			}

			p.mu.Lock()
			p.sessions[key] = sess
			p.mu.Unlock()

			go p.handleSession(conn, sess, key)
		}

		sess.lastActive = time.Now()
		if _, err := sess.tunnelConn.Write(buf[:n]); err != nil {
			p.logger.Warn("udp write to tunnel failed", "client", key, "err", err)
			p.removeSession(key)
		}
	}
}

func (p *UDPProxy) handleSession(conn *net.UDPConn, sess *udpSession, key string) {
	buf := make([]byte, 65536)
	for {
		_ = sess.tunnelConn.SetReadDeadline(time.Now().Add(p.idleTimeout))
		n, err := sess.tunnelConn.Read(buf)
		if err != nil {
			if !util.IsTimeout(err) {
				p.logger.Debug("udp tunnel read error", "client", key, "err", err)
			}
			break
		}

		sess.lastActive = time.Now()
		if _, err := conn.WriteToUDP(buf[:n], sess.clientAddr); err != nil {
			p.logger.Warn("udp write to client failed", "client", key, "err", err)
			break
		}
	}

	p.removeSession(key)
}

func (p *UDPProxy) removeSession(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if sess, exists := p.sessions[key]; exists {
		_ = sess.tunnelConn.Close()
		delete(p.sessions, key)
		p.logger.Debug("udp session removed", "client", key)
	}
}

func (p *UDPProxy) cleanupLoop() {
	ticker := time.NewTicker(p.idleTimeout / 2)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		p.mu.Lock()
		for key, sess := range p.sessions {
			if now.Sub(sess.lastActive) > p.idleTimeout {
				_ = sess.tunnelConn.Close()
				delete(p.sessions, key)
				p.logger.Debug("udp session expired", "client", key)
			}
		}
		p.mu.Unlock()
	}
}
