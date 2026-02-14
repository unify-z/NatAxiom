package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"time"

	"github.com/unify-z/NatAxiom/internal/config"
	"github.com/unify-z/NatAxiom/internal/proto"
	"github.com/unify-z/NatAxiom/internal/util"
)

type Client struct {
	cfg     config.ClientConfig
	logger  *slog.Logger
	tunnels map[string]config.TunnelConfig
}

func New(cfg config.ClientConfig, logger *slog.Logger) *Client {
	tunnels := make(map[string]config.TunnelConfig)
	for _, t := range cfg.Tunnels {
		tunnels[t.Name] = t
	}
	return &Client{cfg: cfg, logger: logger, tunnels: tunnels}
}

func (c *Client) Run(ctx context.Context) error {
	backoff := c.cfg.Client.ReconnectBackoff()
	maxBackoff := c.cfg.Client.MaxReconnectBackoff()
	if maxBackoff < backoff {
		maxBackoff = backoff
	}

	for {
		if err := c.runOnce(ctx); err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return ctx.Err()
			}
			c.logger.Warn("session ended", "err", err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = time.Duration(math.Min(float64(maxBackoff), float64(backoff*2)))
	}
}

func (c *Client) runOnce(ctx context.Context) error {
	controlAddr := fmt.Sprintf("%s:%d", c.cfg.Client.ServerAddr, c.cfg.Client.ControlPort)
	conn, err := net.DialTimeout("tcp", controlAddr, c.cfg.Client.DialTimeout())
	if err != nil {
		return err
	}
	util.EnableTCPKeepAlive(conn)

	defer conn.Close()
	pconn := proto.NewConn(conn)

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := pconn.WriteMessage(proto.Auth{Token: c.cfg.Client.Token}); err != nil {
		return err
	}
	env, err := pconn.ReadEnvelope()
	if err != nil {
		return err
	}
	if env.Type != proto.TypeAuthResp {
		return errors.New("unexpected auth response")
	}
	var authResp proto.AuthResp
	if err := proto.Decode(env, &authResp); err != nil {
		return err
	}
	if !authResp.OK {
		return fmt.Errorf("auth failed: %s", authResp.Error)
	}
	sessionID := authResp.SessionID
	if sessionID == "" {
		return errors.New("missing session id")
	}
	clientID := authResp.ClientID
	if clientID == "" {
		return errors.New("missing client id")
	}

	req := proto.Register{Tunnels: c.buildRegister()}
	if err := pconn.WriteMessage(req); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	env, err = pconn.ReadEnvelope()
	if err != nil {
		return err
	}
	if env.Type != proto.TypeRegisterResp {
		return errors.New("unexpected register response")
	}
	var regResp proto.RegisterResp
	if err := proto.Decode(env, &regResp); err != nil {
		return err
	}
	if !regResp.OK {
		return fmt.Errorf("register failed: %s", regResp.Error)
	}

	c.logger.Info("connected", "server", controlAddr, "tunnels", len(req.Tunnels), "session", sessionID, "client_id", clientID)

	heartbeat := time.NewTicker(c.cfg.Client.HeartbeatInterval())
	defer heartbeat.Stop()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeat.C:
				_ = pconn.WriteMessage(proto.Ping{UnixMillis: time.Now().UnixMilli()})
			}
		}
	}()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(c.cfg.Client.HeartbeatInterval() * 3))
		env, err := pconn.ReadEnvelope()
		if err != nil {
			return err
		}
		switch env.Type {
		case proto.TypeOpen:
			var open proto.Open
			if err := proto.Decode(env, &open); err != nil {
				continue
			}
			go c.handleOpen(open, sessionID)
		case proto.TypePong:
		case proto.TypeError:
			var msg proto.Error
			if err := proto.Decode(env, &msg); err == nil {
				c.logger.Warn("server error", "message", msg.Message)
			}
		default:
			c.logger.Warn("unexpected control message", "type", env.Type)
		}
	}
}

func (c *Client) buildRegister() []proto.TunnelSpec {
	res := make([]proto.TunnelSpec, 0, len(c.tunnels))
	for _, t := range c.tunnels {
		spec := proto.TunnelSpec{
			Name:       t.Name,
			Type:       t.Type,
			RemotePort: t.RemotePort,
			Domain:     t.Domain,
		}

		if t.Type == "https" && t.CertFile != "" && t.KeyFile != "" {
			certPEM, err := os.ReadFile(t.CertFile)
			if err != nil {
				c.logger.Warn("failed to read cert file", "tunnel", t.Name, "file", t.CertFile, "err", err)
				continue
			}
			keyPEM, err := os.ReadFile(t.KeyFile)
			if err != nil {
				c.logger.Warn("failed to read key file", "tunnel", t.Name, "file", t.KeyFile, "err", err)
				continue
			}
			spec.CertPEM = string(certPEM)
			spec.KeyPEM = string(keyPEM)
		}

		res = append(res, spec)
	}
	return res
}

func (c *Client) handleOpen(open proto.Open, sessionID string) {
	tunnel, ok := c.tunnels[open.TunnelName]
	if !ok {
		c.logger.Warn("unknown tunnel", "tunnel", open.TunnelName)
		return
	}

	c.logger.Debug("open received", "tunnel", open.TunnelName, "conn_id", open.ConnID)

	dataAddr := fmt.Sprintf("%s:%d", c.cfg.Client.ServerAddr, c.cfg.Client.DataPort)
	dataConn, err := net.DialTimeout("tcp", dataAddr, c.cfg.Client.DialTimeout())
	if err != nil {
		c.logger.Warn("failed to connect data", "addr", dataAddr, "err", err)
		return
	}
	util.EnableTCPKeepAlive(dataConn)

	if err := proto.WriteMessage(dataConn, proto.DataHello{SessionID: sessionID, ConnID: open.ConnID}); err != nil {
		_ = dataConn.Close()
		c.logger.Warn("failed to send data hello", "err", err)
		return
	}

	switch tunnel.Type {
	case "tcp", "":
		c.handleTCPTunnel(tunnel, dataConn, open.ConnID)
	case "udp":
		c.handleUDPTunnel(tunnel, dataConn, open.ConnID)
	case "http", "https":
		c.handleHTTPTunnel(tunnel, dataConn, open.ConnID)
	default:
		c.logger.Warn("unsupported tunnel type", "tunnel", tunnel.Name, "type", tunnel.Type)
		_ = dataConn.Close()
	}
}

func (c *Client) handleTCPTunnel(tunnel config.TunnelConfig, dataConn net.Conn, connID string) {
	localConn, err := net.DialTimeout("tcp", tunnel.LocalAddr, c.cfg.Client.DialTimeout())
	if err != nil {
		_ = dataConn.Close()
		c.logger.Warn("failed to connect local", "tunnel", tunnel.Name, "addr", tunnel.LocalAddr, "err", err)
		return
	}
	util.EnableTCPKeepAlive(localConn)

	c.logger.Debug("tcp bridge start", "tunnel", tunnel.Name, "conn_id", connID, "local", tunnel.LocalAddr)
	label := tunnel.Name + ":" + connID
	util.ProxyWithLogger(localConn, dataConn, c.logger, label)
	c.logger.Debug("tcp bridge closed", "tunnel", tunnel.Name, "conn_id", connID)
}

func (c *Client) handleUDPTunnel(tunnel config.TunnelConfig, dataConn net.Conn, connID string) {
	localAddr, err := net.ResolveUDPAddr("udp", tunnel.LocalAddr)
	if err != nil {
		_ = dataConn.Close()
		c.logger.Warn("failed to resolve local udp addr", "tunnel", tunnel.Name, "addr", tunnel.LocalAddr, "err", err)
		return
	}

	localConn, err := net.DialUDP("udp", nil, localAddr)
	if err != nil {
		_ = dataConn.Close()
		c.logger.Warn("failed to connect local udp", "tunnel", tunnel.Name, "addr", tunnel.LocalAddr, "err", err)
		return
	}

	c.logger.Debug("udp bridge start", "tunnel", tunnel.Name, "conn_id", connID, "local", tunnel.LocalAddr)
	label := tunnel.Name + ":" + connID
	util.UDPProxy(localConn, dataConn, c.logger, label, c.cfg.Client.UdpIdleTimeout())
	c.logger.Debug("udp bridge closed", "tunnel", tunnel.Name, "conn_id", connID)
}

func (c *Client) handleHTTPTunnel(tunnel config.TunnelConfig, dataConn net.Conn, connID string) {
	localConn, err := net.DialTimeout("tcp", tunnel.LocalAddr, c.cfg.Client.DialTimeout())
	if err != nil {
		_ = dataConn.Close()
		c.logger.Warn("failed to connect local", "tunnel", tunnel.Name, "addr", tunnel.LocalAddr, "err", err)
		return
	}
	util.EnableTCPKeepAlive(localConn)

	c.logger.Debug("http bridge start", "tunnel", tunnel.Name, "conn_id", connID, "local", tunnel.LocalAddr)
	label := tunnel.Name + ":" + connID
	util.ProxyWithLogger(localConn, dataConn, c.logger, label)
	c.logger.Debug("http bridge closed", "tunnel", tunnel.Name, "conn_id", connID)
}
