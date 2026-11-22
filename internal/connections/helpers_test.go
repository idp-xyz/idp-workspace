package connections_test

import (
	"crypto/sha256"
	"encoding/hex"
)

func sha256hex(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
