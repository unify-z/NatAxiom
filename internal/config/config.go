package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type ServerConfig struct {
	Server ServerSettings `toml:"server"`
	HTTP   HTTPSettings   `toml:"http"`
	HTTPS  HTTPSSettings  `toml:"https"`
}

type ServerSettings struct {
	BindAddr            string `toml:"bind_addr"`
	ControlPort         int    `toml:"control_port"`
	DataPort            int    `toml:"data_port"`
	Token               string `toml:"token"`
	ReadTimeoutSecs     int    `toml:"read_timeout_secs"`
	WriteTimeoutSecs    int    `toml:"write_timeout_secs"`
	PendingTimeoutSecs  int    `toml:"pending_timeout_secs"`
	LogLevel            string `toml:"log_level"`
	MaxPendingPerTunnel int    `toml:"max_pending_per_tunnel"`
	UdpIdleSecs         int    `toml:"udp_idle_secs"`
}

type HTTPSettings struct {
	BindAddr string `toml:"bind_addr"`
	Port     int    `toml:"port"`
}

type HTTPSSettings struct {
	BindAddr string `toml:"bind_addr"`
	Port     int    `toml:"port"`
}

type ClientConfig struct {
	Client  ClientSettings `toml:"client"`
	Tunnels []TunnelConfig `toml:"tunnels"`
}

type ClientSettings struct {
	ServerAddr             string `toml:"server_addr"`
	ControlPort            int    `toml:"control_port"`
	DataPort               int    `toml:"data_port"`
	Token                  string `toml:"token"`
	HeartbeatIntervalSecs  int    `toml:"heartbeat_interval_secs"`
	DialTimeoutSecs        int    `toml:"dial_timeout_secs"`
	ReconnectBackoffSecs   int    `toml:"reconnect_backoff_secs"`
	MaxReconnectBackoffSec int    `toml:"max_reconnect_backoff_secs"`
	LogLevel               string `toml:"log_level"`
	UdpIdleSecs            int    `toml:"udp_idle_secs"`
}

type TunnelConfig struct {
	Name       string `toml:"name"`
	Type       string `toml:"type"`
	RemotePort int    `toml:"remote_port"`
	LocalAddr  string `toml:"local_addr"`
	Domain     string `toml:"domain"`
	CertFile   string `toml:"cert_file"`
	KeyFile    string `toml:"key_file"`
}

func LoadServerConfig(path string) (ServerConfig, error) {
	cfg := ServerConfig{}
	if err := decodeToml(path, &cfg); err != nil {
		return cfg, err
	}
	applyServerDefaults(&cfg.Server)
	applyHTTPDefaults(&cfg.HTTP, cfg.Server.BindAddr)
	applyHTTPSDefaults(&cfg.HTTPS, cfg.Server.BindAddr)
	if err := validateServer(cfg.Server); err != nil {
		return cfg, err
	}
	if err := validateHTTP(cfg.HTTP); err != nil {
		return cfg, err
	}
	if err := validateHTTPS(cfg.HTTPS); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func LoadClientConfig(path string) (ClientConfig, error) {
	cfg := ClientConfig{}
	if err := decodeToml(path, &cfg); err != nil {
		return cfg, err
	}
	applyClientDefaults(&cfg.Client)
	normalizeTunnelTypes(cfg.Tunnels)
	if err := validateClient(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func decodeToml(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	dec := toml.NewDecoder(f)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("toml decode: %w", err)
	}
	return nil
}

func applyServerDefaults(s *ServerSettings) {
	if s.BindAddr == "" {
		s.BindAddr = "0.0.0.0"
	}
	if s.ControlPort == 0 {
		s.ControlPort = 7000
	}
	if s.DataPort == 0 {
		s.DataPort = 7001
	}
	if s.ReadTimeoutSecs == 0 {
		s.ReadTimeoutSecs = 60
	}
	if s.WriteTimeoutSecs == 0 {
		s.WriteTimeoutSecs = 60
	}
	if s.PendingTimeoutSecs == 0 {
		s.PendingTimeoutSecs = 30
	}
	if s.LogLevel == "" {
		s.LogLevel = "info"
	}
	if s.MaxPendingPerTunnel == 0 {
		s.MaxPendingPerTunnel = 1024
	}
	if s.UdpIdleSecs == 0 {
		s.UdpIdleSecs = 60
	}
}

func applyHTTPDefaults(h *HTTPSettings, bindAddr string) {
	if h.BindAddr == "" {
		h.BindAddr = bindAddr
	}
}

func applyHTTPSDefaults(h *HTTPSSettings, bindAddr string) {
	if h.BindAddr == "" {
		h.BindAddr = bindAddr
	}
}

func applyClientDefaults(c *ClientSettings) {
	if c.ControlPort == 0 {
		c.ControlPort = 7000
	}
	if c.DataPort == 0 {
		c.DataPort = 7001
	}
	if c.HeartbeatIntervalSecs == 0 {
		c.HeartbeatIntervalSecs = 15
	}
	if c.DialTimeoutSecs == 0 {
		c.DialTimeoutSecs = 5
	}
	if c.ReconnectBackoffSecs == 0 {
		c.ReconnectBackoffSecs = 2
	}
	if c.MaxReconnectBackoffSec == 0 {
		c.MaxReconnectBackoffSec = 30
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.UdpIdleSecs == 0 {
		c.UdpIdleSecs = 60
	}
}

func validateServer(s ServerSettings) error {
	if s.Token == "" {
		return errors.New("server.token is required")
	}
	if s.ControlPort <= 0 || s.ControlPort > 65535 {
		return errors.New("server.control_port must be 1-65535")
	}
	if s.DataPort <= 0 || s.DataPort > 65535 {
		return errors.New("server.data_port must be 1-65535")
	}
	if s.ControlPort == s.DataPort {
		return errors.New("server.control_port and server.data_port must differ")
	}
	if s.PendingTimeoutSecs < 5 {
		return errors.New("server.pending_timeout_secs must be >= 5")
	}
	if s.MaxPendingPerTunnel < 1 {
		return errors.New("server.max_pending_per_tunnel must be >= 1")
	}
	if s.UdpIdleSecs < 5 {
		return errors.New("server.udp_idle_secs must be >= 5")
	}
	return nil
}

func validateHTTP(h HTTPSettings) error {
	if h.Port < 0 || h.Port > 65535 {
		return errors.New("http.port must be 0-65535")
	}
	if h.Port > 0 && h.BindAddr == "" {
		return errors.New("http.bind_addr is required when http.port > 0")
	}
	return nil
}

func validateHTTPS(h HTTPSSettings) error {
	if h.Port < 0 || h.Port > 65535 {
		return errors.New("https.port must be 0-65535")
	}
	if h.Port > 0 && h.BindAddr == "" {
		return errors.New("https.bind_addr is required when https.port > 0")
	}
	return nil
}

func validateClient(cfg ClientConfig) error {
	c := cfg.Client
	if c.ServerAddr == "" {
		return errors.New("client.server_addr is required")
	}
	if c.Token == "" {
		return errors.New("client.token is required")
	}
	if c.ControlPort <= 0 || c.ControlPort > 65535 {
		return errors.New("client.control_port must be 1-65535")
	}
	if c.DataPort <= 0 || c.DataPort > 65535 {
		return errors.New("client.data_port must be 1-65535")
	}
	if c.ControlPort == c.DataPort {
		return errors.New("client.control_port and client.data_port must differ")
	}
	if c.UdpIdleSecs < 5 {
		return errors.New("client.udp_idle_secs must be >= 5")
	}
	if len(cfg.Tunnels) == 0 {
		return errors.New("at least one [[tunnels]] entry is required")
	}
	seen := map[string]struct{}{}
	for i, t := range cfg.Tunnels {
		if t.Name == "" {
			return fmt.Errorf("tunnels[%d].name is required", i)
		}
		if _, ok := seen[t.Name]; ok {
			return fmt.Errorf("duplicate tunnel name: %s", t.Name)
		}
		seen[t.Name] = struct{}{}
		tt := normalizeTunnelType(t.Type)
		switch tt {
		case "tcp", "udp":
			if t.RemotePort <= 0 || t.RemotePort > 65535 {
				return fmt.Errorf("tunnels[%d].remote_port must be 1-65535", i)
			}
			if t.LocalAddr == "" {
				return fmt.Errorf("tunnels[%d].local_addr is required", i)
			}
		case "http", "https":
			if t.Domain == "" {
				return fmt.Errorf("tunnels[%d].domain is required for %s", i, tt)
			}
			if t.LocalAddr == "" {
				return fmt.Errorf("tunnels[%d].local_addr is required", i)
			}
			if tt == "https" {
				if t.CertFile == "" {
					return fmt.Errorf("tunnels[%d].cert_file is required for https", i)
				}
				if t.KeyFile == "" {
					return fmt.Errorf("tunnels[%d].key_file is required for https", i)
				}
			}
		default:
			return fmt.Errorf("tunnels[%d].type must be tcp|udp|http|https", i)
		}
	}
	return nil
}

func normalizeTunnelTypes(tunnels []TunnelConfig) {
	for i := range tunnels {
		if tunnels[i].Type == "" {
			tunnels[i].Type = "tcp"
		}
	}
}

func normalizeTunnelType(t string) string {
	if t == "" {
		return "tcp"
	}
	switch t {
	case "tcp", "udp", "http", "https":
		return t
	default:
		return t
	}
}

func (s ServerSettings) ReadTimeout() time.Duration {
	return time.Duration(s.ReadTimeoutSecs) * time.Second
}

func (s ServerSettings) WriteTimeout() time.Duration {
	return time.Duration(s.WriteTimeoutSecs) * time.Second
}

func (s ServerSettings) PendingTimeout() time.Duration {
	return time.Duration(s.PendingTimeoutSecs) * time.Second
}

func (c ClientSettings) HeartbeatInterval() time.Duration {
	return time.Duration(c.HeartbeatIntervalSecs) * time.Second
}

func (c ClientSettings) DialTimeout() time.Duration {
	return time.Duration(c.DialTimeoutSecs) * time.Second
}

func (c ClientSettings) ReconnectBackoff() time.Duration {
	return time.Duration(c.ReconnectBackoffSecs) * time.Second
}

func (c ClientSettings) MaxReconnectBackoff() time.Duration {
	return time.Duration(c.MaxReconnectBackoffSec) * time.Second
}

func (s ServerSettings) UdpIdleTimeout() time.Duration {
	return time.Duration(s.UdpIdleSecs) * time.Second
}

func (c ClientSettings) UdpIdleTimeout() time.Duration {
	return time.Duration(c.UdpIdleSecs) * time.Second
}
