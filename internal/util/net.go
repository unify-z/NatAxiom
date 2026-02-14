package util

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

func EnableTCPKeepAlive(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(30 * time.Second)
	}
}

func Proxy(a, b net.Conn) {
	proxy(a, b, nil, "")
}

func ProxyWithLogger(a, b net.Conn, logger *slog.Logger, label string) {
	proxy(a, b, logger, label)
}

func proxy(a, b net.Conn, logger *slog.Logger, label string) {
	if logger != nil {
		logger.Debug(
			"proxy start",
			"label", label,
			"a_local", addrString(a),
			"a_remote", remoteString(a),
			"b_local", addrString(b),
			"b_remote", remoteString(b),
		)
	}
	var wg sync.WaitGroup
	wg.Add(2)

	copyFunc := func(dst, src net.Conn, dir string) {
		defer wg.Done()
		n, err := io.Copy(dst, src)
		if logger != nil {
			logger.Debug(
				"proxy copy",
				"label", label,
				"dir", dir,
				"bytes", n,
				"err", err,
			)
		}
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}

	go copyFunc(a, b, "b->a")
	go copyFunc(b, a, "a->b")
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
	if logger != nil {
		logger.Debug("proxy done", "label", label)
	}
}

func addrString(conn net.Conn) string {
	if conn == nil || conn.LocalAddr() == nil {
		return ""
	}
	return conn.LocalAddr().String()
}

func remoteString(conn net.Conn) string {
	if conn == nil || conn.RemoteAddr() == nil {
		return ""
	}
	return conn.RemoteAddr().String()
}
