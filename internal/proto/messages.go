package proto

type Message interface {
	MessageType() string
}

const (
	TypeAuth         = "auth"
	TypeAuthResp     = "auth_resp"
	TypeRegister     = "register"
	TypeRegisterResp = "register_resp"
	TypeOpen         = "open"
	TypeDataHello    = "data_hello"
	TypePing         = "ping"
	TypePong         = "pong"
	TypeError        = "error"
)

type Auth struct {
	Token string `json:"token"`
}

func (Auth) MessageType() string { return TypeAuth }

type AuthResp struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	ClientID  string `json:"client_id,omitempty"`
}

func (AuthResp) MessageType() string { return TypeAuthResp }

type Register struct {
	Tunnels []TunnelSpec `json:"tunnels"`
}

func (Register) MessageType() string { return TypeRegister }

type RegisterResp struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (RegisterResp) MessageType() string { return TypeRegisterResp }

type TunnelSpec struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	RemotePort int    `json:"remote_port"`
	LocalAddr  string `json:"local_addr,omitempty"`
	Domain     string `json:"domain,omitempty"`
	CertPEM    string `json:"cert_pem,omitempty"`
	KeyPEM     string `json:"key_pem,omitempty"`
}

type Open struct {
	TunnelName string `json:"tunnel_name"`
	ConnID     string `json:"conn_id"`
}

func (Open) MessageType() string { return TypeOpen }

type DataHello struct {
	SessionID string `json:"session_id"`
	ConnID    string `json:"conn_id"`
}

func (DataHello) MessageType() string { return TypeDataHello }

type Ping struct {
	UnixMillis int64 `json:"unix_millis"`
}

func (Ping) MessageType() string { return TypePing }

type Pong struct {
	UnixMillis int64 `json:"unix_millis"`
}

func (Pong) MessageType() string { return TypePong }

type Error struct {
	Message string `json:"message"`
}

func (Error) MessageType() string { return TypeError }
