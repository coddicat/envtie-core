package plan

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/coddicat/envtie-core/core"
)

func TestPlanSignatureAndWorkspace(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := core.Plan{ID: "test", Environment: "preview", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}
	if err := Sign(&p, "workspace-a", priv); err != nil {
		t.Fatal(err)
	}
	if err := Verify(p, "workspace-a", pub, time.Now()); err != nil {
		t.Fatal(err)
	}
	if Verify(p, "workspace-b", pub, time.Now()) == nil {
		t.Fatal("cross-workspace replay accepted")
	}
	p.Environment = "other"
	if Verify(p, "workspace-a", pub, time.Now()) == nil {
		t.Fatal("tampered plan accepted")
	}
}
