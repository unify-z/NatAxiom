package proto

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
)

type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

const MaxMessageSize = 1 << 20

func WriteMessage(w io.Writer, msg Message) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	env := Envelope{Type: msg.MessageType(), Payload: payload}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return errors.New("empty message")
	}
	if len(data) > MaxMessageSize {
		return errors.New("message too large")
	}
	var lenbuf [4]byte
	binary.BigEndian.PutUint32(lenbuf[:], uint32(len(data)))
	if _, err := w.Write(lenbuf[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func ReadEnvelope(r io.Reader) (Envelope, error) {
	var lenbuf [4]byte
	if _, err := io.ReadFull(r, lenbuf[:]); err != nil {
		return Envelope{}, err
	}
	size := binary.BigEndian.Uint32(lenbuf[:])
	if size == 0 {
		return Envelope{}, errors.New("invalid message size")
	}
	if size > MaxMessageSize {
		return Envelope{}, errors.New("message too large")
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return Envelope{}, err
	}
	var env Envelope
	if err := json.Unmarshal(buf, &env); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

func Decode(env Envelope, v any) error {
	if len(env.Payload) == 0 {
		return errors.New("empty payload")
	}
	return json.Unmarshal(env.Payload, v)
}

type Conn struct {
	c   net.Conn
	r   *bufio.Reader
	w   *bufio.Writer
	wmu sync.Mutex
}

func NewConn(c net.Conn) *Conn {
	return &Conn{
		c: c,
		r: bufio.NewReader(c),
		w: bufio.NewWriter(c),
	}
}

func (c *Conn) ReadEnvelope() (Envelope, error) {
	return ReadEnvelope(c.r)
}

func (c *Conn) WriteMessage(msg Message) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := WriteMessage(c.w, msg); err != nil {
		return err
	}
	return c.w.Flush()
}

func (c *Conn) Close() error { return c.c.Close() }

func (c *Conn) RawConn() net.Conn { return c.c }
