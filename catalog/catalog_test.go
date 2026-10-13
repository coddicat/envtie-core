package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildCatalogsReleaseNamedAdapters(t *testing.T) {
	dir := t.TempDir()
	body := []byte("neon build")
	for name, data := range map[string][]byte{
		"adapter-neon_sha-1_linux_amd64":       body,
		"adapter-neon_sha-1_windows_arm64.exe": body,
		"adapter-neon_sha-0_linux_amd64":       []byte("older release"),
		"adapter-neon_sha-1_windows_amd64":     []byte("missing .exe"),
		"adapters.json":                        []byte("{}"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := Build(dir, "sha-1")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	targets := catalog.Adapters["neon@1"]
	if len(catalog.Adapters) != 1 || len(targets) != 2 {
		t.Fatalf("catalog=%+v", catalog)
	}
	pin := targets["linux/amd64"]
	if pin.SHA256 != hex.EncodeToString(sum[:]) || pin.Size != int64(len(body)) || pin.URL != "/v1/adapter-artifacts/neon@1/linux/amd64" {
		t.Fatalf("pin=%+v", pin)
	}
	if _, err := Build(dir, "sha-2"); err == nil {
		t.Fatal("expected an empty release to be rejected")
	}
}

func TestParseAndEnforceOfficialPins(t *testing.T) {
	catalog, err := Parse([]byte(`{
		"release": "v2.0.0",
		"base_url": "https://releases.example/v2.0.0",
		"adapters": {
			"neon@1": {"linux/amd64": {"sha256": "` + strings.Repeat("a", 64) + `"}},
			"upstash@1": {"windows/amd64": {"sha256": "` + strings.Repeat("b", 64) + `"}}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Enforce("neon@1", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	resolved, err := catalog.Resolve("neon@1")
	if err != nil || resolved["linux/amd64"].URL != "https://releases.example/v2.0.0/adapter-neon_v2.0.0_linux_amd64" {
		t.Fatalf("resolved=%+v err=%v", resolved, err)
	}
	if err := catalog.Enforce("neon@1", strings.Repeat("c", 64)); err == nil {
		t.Fatal("expected an unlisted pin to be rejected")
	}
	if err := catalog.Enforce("custom@1", strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]byte(`{"release":"v2","base_url":"https://example.invalid","adapters":{"evil@1":{"linux/amd64":{"sha256":"` + strings.Repeat("d", 64) + `"}}}}`)); err == nil {
		t.Fatal("expected an unknown adapter to be rejected")
	}
	if err := (Catalog{}).Enforce("neon@1", "short"); err != nil {
		t.Fatal("an empty catalog does not enforce pins")
	}
}
