package plan

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/coddicat/envtie-core/core"
)

func bytesToSign(p core.Plan, workspace string) ([]byte, error) {
	p.Signature = ""
	return json.Marshal(struct {
		Workspace string    `json:"workspace"`
		Plan      core.Plan `json:"plan"`
	}{workspace, p})
}

func Sign(p *core.Plan, workspace string, key ed25519.PrivateKey) error {
	b, err := bytesToSign(*p, workspace)
	if err != nil {
		return err
	}
	p.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, b))
	return nil
}

func Verify(p core.Plan, workspace string, key ed25519.PublicKey, now time.Time) error {
	if now.After(p.ExpiresAt) || now.Before(p.CreatedAt.Add(-time.Minute)) {
		return errors.New("plan expired or not yet valid")
	}
	b, err := bytesToSign(p, workspace)
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(p.Signature)
	if err != nil || !ed25519.Verify(key, b, sig) {
		return errors.New("plan signature invalid")
	}
	return nil
}

func Fingerprint(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:12])
}
