package util

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"
)

func NewID() string {
	var b [16]byte
	_, err := rand.Read(b[:])
	if err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}
