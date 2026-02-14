package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/unify-z/NatAxiom/internal/config"
	"github.com/unify-z/NatAxiom/internal/proto"
	"github.com/unify-z/NatAxiom/internal/util"
)

type Server struct {
	cfg          config.ServerConfig
	logger       *slog.Logger
	controlLn    net.Listener
	dataLn       net.Listener
	portRegistry *portRegistry
	httpProxy    *HTTPProxy
	sessions     sync.Map
}

func New(cfg config.ServerConfig, logger *slog.Logger) *Server {
	httpProxy := NewHTTPProxy(cfg, logger, &sync.Map{})
	return &Server{
		cfg:          cfg,
		logger:       logger,
		portRegistry: newPortRegistry(),
		httpProxy:    httpProxy,
	}
}

func (s *Server) Serve(ctx context.Context) error {
	controlAddr := fmt.Sprintf("%s:%d", s.cfg.Server.BindAddr, s.cfg.Server.ControlPort)
	dataAddr := fmt.Sprintf("%s:%d", s.cfg.Server.BindAddr, s.cfg.Server.DataPort)

	controlLn, err := net.Listen("tcp", controlAddr)
	if err != nil {
		return fmt.Errorf("listen control: %w", err)
	}
	defer controlLn.Close()
	s.controlLn = controlLn

	dataLn, err := net.Listen("tcp", dataAddr)
	if err != nil {
		return fmt.Errorf("listen data: %w", err)
	}
	defer dataLn.Close()
	s.dataLn = dataLn

	s.logger.Info("server listening",
		"control", controlAddr,
		"data", dataAddr,
	)

	s.httpProxy.sessions = &s.sessions

	go s.acceptControl(ctx)
	go s.acceptData(ctx)

	if s.cfg.HTTP.Port > 0 {
		go func() {
			if err := s.httpProxy.ServeHTTP(ctx); err != nil {
				s.logger.Error("http proxy failed", "err", err)
			}
		}()
	}

	if s.cfg.HTTPS.Port > 0 {
		go func() {
			if err := s.httpProxy.ServeHTTPS(ctx); err != nil {
				s.logger.Error("https proxy failed", "err", err)
			}
		}()
	}

	<-ctx.Done()
	return ctx.Err()
}

func (s *Server) acceptControl(ctx context.Context) {
	for {
		conn, err := s.controlLn.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			s.logger.Warn("control accept failed", "err", err)
			continue
		}
		util.EnableTCPKeepAlive(conn)
		go s.handleControl(ctx, conn)
	}
}

func (s *Server) acceptData(ctx context.Context) {
	for {
		conn, err := s.dataLn.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			s.logger.Warn("data accept failed", "err", err)
			continue
		}
		util.EnableTCPKeepAlive(conn)
		go s.handleData(conn)
	}
}

func (s *Server) handleControl(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	pconn := proto.NewConn(conn)
	_ = conn.SetReadDeadline(time.Now().Add(s.cfg.Server.ReadTimeout()))

	env, err := pconn.ReadEnvelope()
	if err != nil {
		s.logger.Warn("failed to read auth", "err", err)
		return
	}
	if env.Type != proto.TypeAuth {
		_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.Server.WriteTimeout()))
		_ = pconn.WriteMessage(proto.Error{Message: "expected auth"})
		return
	}
	var auth proto.Auth
	if err := proto.Decode(env, &auth); err != nil {
		_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.Server.WriteTimeout()))
		_ = pconn.WriteMessage(proto.Error{Message: "invalid auth payload"})
		return
	}
	if auth.Token != s.cfg.Server.Token {
		_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.Server.WriteTimeout()))
		_ = pconn.WriteMessage(proto.AuthResp{OK: false, Error: "invalid token"})
		return
	}

	sessionID := util.NewID()
	if sessionID == "" {
		_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.Server.WriteTimeout()))
		_ = pconn.WriteMessage(proto.AuthResp{OK: false, Error: "failed to generate session id"})
		return
	}

	clientID := util.NewID()
	if clientID == "" {
		_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.Server.WriteTimeout()))
		_ = pconn.WriteMessage(proto.AuthResp{OK: false, Error: "failed to generate client id"})
		return
	}

	sessLogger := s.logger.With("session", sessionID, "client", clientID)
	sess := newSession(sessionID, clientID, s.cfg.Server, sessLogger, pconn, s.portRegistry, s.httpProxy, func() {
		s.sessions.Delete(sessionID)
	})
	s.sessions.Store(sessionID, sess)
	defer sess.Close()

	_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.Server.WriteTimeout()))
	if err := pconn.WriteMessage(proto.AuthResp{OK: true, SessionID: sessionID, ClientID: clientID}); err != nil {
		sessLogger.Warn("failed to send auth response", "err", err)
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(s.cfg.Server.ReadTimeout()))
	env, err = pconn.ReadEnvelope()
	if err != nil {
		sessLogger.Warn("failed to read register", "err", err)
		return
	}
	if env.Type != proto.TypeRegister {
		_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.Server.WriteTimeout()))
		_ = pconn.WriteMessage(proto.RegisterResp{OK: false, Error: "expected register"})
		return
	}
	var reg proto.Register
	if err := proto.Decode(env, &reg); err != nil {
		_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.Server.WriteTimeout()))
		_ = pconn.WriteMessage(proto.RegisterResp{OK: false, Error: "invalid register payload"})
		return
	}

	if err := sess.Register(reg.Tunnels); err != nil {
		_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.Server.WriteTimeout()))
		_ = pconn.WriteMessage(proto.RegisterResp{OK: false, Error: err.Error()})
		return
	}

	_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.Server.WriteTimeout()))
	if err := pconn.WriteMessage(proto.RegisterResp{OK: true}); err != nil {
		sessLogger.Warn("failed to send register response", "err", err)
		return
	}

	sessLogger.Info("client registered", "tunnels", len(reg.Tunnels))

	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.cfg.Server.ReadTimeout()))
		env, err := pconn.ReadEnvelope()
		if err != nil {
			sessLogger.Warn("control connection closed", "err", err)
			return
		}
		switch env.Type {
		case proto.TypePing:
			var ping proto.Ping
			if err := proto.Decode(env, &ping); err != nil {
				continue
			}
			_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.Server.WriteTimeout()))
			_ = pconn.WriteMessage(proto.Pong{UnixMillis: ping.UnixMillis})
		case proto.TypeError:
			var msg proto.Error
			if err := proto.Decode(env, &msg); err == nil {
				sessLogger.Warn("client error", "message", msg.Message)
			}
		default:
			sessLogger.Warn("unexpected control message", "type", env.Type)
		}
	}
}

func (s *Server) handleData(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	env, err := proto.ReadEnvelope(conn)
	if err != nil {
		s.logger.Warn("data handshake failed", "err", err)
		_ = conn.Close()
		return
	}
	if env.Type != proto.TypeDataHello {
		s.logger.Warn("unexpected data message", "type", env.Type)
		_ = conn.Close()
		return
	}
	var hello proto.DataHello
	if err := proto.Decode(env, &hello); err != nil {
		s.logger.Warn("invalid data hello", "err", err)
		_ = conn.Close()
		return
	}

	value, ok := s.sessions.Load(hello.SessionID)
	if !ok {
		s.logger.Warn("unknown session", "session", hello.SessionID)
		_ = conn.Close()
		return
	}
	sess := value.(*session)
	_ = conn.SetReadDeadline(time.Time{})
	if !sess.AttachData(hello.ConnID, conn) {
		s.logger.Warn("no pending connection", "session", hello.SessionID, "conn_id", hello.ConnID, "remote", conn.RemoteAddr().String())
		_ = conn.Close()
		return
	}
}
