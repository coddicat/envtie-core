package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"
)

func VerifyChecksum(path, wanted string) error {
	if len(wanted) != 64 {
		return NewError("adapter_pin_required")
	}
	f, err := os.Open(path)
	if err != nil {
		return NewError("adapter_executable_missing")
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return NewError("adapter_executable_missing")
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), wanted) {
		return NewError("adapter_checksum_mismatch")
	}
	return nil
}
