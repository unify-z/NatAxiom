package util

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

func UDPProxy(local, remote net.Conn, logger *slog.Logger, label string, idleTimeout time.Duration) {
	if logger != nil {
		logger.Debug("udp proxy start", "label", label)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	copyFunc := func(dst, src net.Conn, dir string) {
		defer wg.Done()
		buf := make([]byte, 65536)
		var n int64
		for {
			_ = src.SetReadDeadline(time.Now().Add(idleTimeout))
			nr, err := src.Read(buf)
			if nr > 0 {
				nw, ew := dst.Write(buf[:nr])
				if nw > 0 {
					n += int64(nw)
				}
				if ew != nil {
					if logger != nil {
						logger.Debug("udp proxy write error", "label", label, "dir", dir, "err", ew)
					}
					break
				}
			}
			if err != nil {
				if err != io.EOF && !isTimeout(err) {
					if logger != nil {
						logger.Debug("udp proxy read error", "label", label, "dir", dir, "err", err)
					}
				}
				break
			}
		}
		if logger != nil {
			logger.Debug("udp proxy copy done", "label", label, "dir", dir, "bytes", n)
		}
	}

	go copyFunc(local, remote, "remote->local")
	go copyFunc(remote, local, "local->remote")
	wg.Wait()
	_ = local.Close()
	_ = remote.Close()
	if logger != nil {
		logger.Debug("udp proxy done", "label", label)
	}
}

func IsTimeout(err error) bool {
	if ne, ok := err.(net.Error); ok {
		return ne.Timeout()
	}
	return false
}

func isTimeout(err error) bool {
	return IsTimeout(err)
}
