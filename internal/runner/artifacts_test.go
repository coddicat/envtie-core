package runner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coddicat/envtie-core/core"
)

func TestOfficialArtifactDownloadsOnceAndUsesCache(t *testing.T) {
	body := []byte("fixture adapter binary")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write(body)
	}))
	defer server.Close()
	t.Setenv("ENVTIE_ADAPTER_CACHE", t.TempDir())
	component := core.Component{
		Adapter: "neon@1",
		Artifacts: map[string]core.AdapterArtifact{
			runtime.GOOS + "/" + runtime.GOARCH: {
				Release: "v1.0.0", URL: server.URL + "/adapter", SHA256: digest, Size: int64(len(body)),
			},
		},
	}
	first, err := executableForComponent(context.Background(), component)
	if err != nil {
		t.Fatal(err)
	}
	second, err := executableForComponent(context.Background(), component)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path != second.Path || first.SHA256 != digest || requests.Load() != 1 {
		t.Fatalf("first=%+v second=%+v requests=%d", first, second, requests.Load())
	}
	if got, err := os.ReadFile(first.Path); err != nil || string(got) != string(body) {
		t.Fatalf("cached body=%q err=%v", got, err)
	}
}

func TestOfficialArtifactRejectsWrongDigestAndMissingPlatform(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("wrong"))
	}))
	defer server.Close()
	t.Setenv("ENVTIE_ADAPTER_CACHE", t.TempDir())
	component := core.Component{
		Adapter: "neon@1",
		Artifacts: map[string]core.AdapterArtifact{
			runtime.GOOS + "/" + runtime.GOARCH: {
				Release: "v1", URL: server.URL, SHA256: strings.Repeat("a", 64),
			},
		},
	}
	if _, err := executableForComponent(context.Background(), component); err == nil || !strings.Contains(err.Error(), "adapter_checksum_mismatch") {
		t.Fatalf("wrong digest: %v", err)
	}
	component.Artifacts = map[string]core.AdapterArtifact{"other/arch": {}}
	if _, err := executableForComponent(context.Background(), component); err == nil || !strings.Contains(err.Error(), "adapter_platform_unavailable") {
		t.Fatalf("missing platform: %v", err)
	}
}

func TestCustomRegistryRequiresTrustedPublisherSignature(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("custom adapter")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	var serverURL string
	release := customRegistryRelease{
		Adapter: "example@1", Publisher: "acme", Version: "1.2.3",
		Artifacts: map[string]core.AdapterArtifact{
			runtime.GOOS + "/" + runtime.GOARCH: {
				Release: "1.2.3", SHA256: digest, Size: int64(len(body)),
			},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/binary") {
			_, _ = w.Write(body)
			return
		}
		copy := release
		artifact := copy.Artifacts[runtime.GOOS+"/"+runtime.GOARCH]
		artifact.URL = serverURL + "/binary"
		copy.Artifacts[runtime.GOOS+"/"+runtime.GOARCH] = artifact
		payload, _ := json.Marshal(copy)
		_ = json.NewEncoder(w).Encode(customRegistryEnvelope{
			Signed: copy, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
		})
	}))
	defer server.Close()
	serverURL = server.URL
	t.Setenv("ENVTIE_ADAPTER_CACHE", t.TempDir())
	trust, _ := json.Marshal(map[string]string{"acme": base64.StdEncoding.EncodeToString(publicKey)})
	t.Setenv("ENVTIE_ADAPTER_TRUST_KEYS", string(trust))
	component := core.Component{
		Adapter: "example@1",
		Source: &core.AdapterSource{
			Kind: "registry", Registry: server.URL, Publisher: "acme", Version: "1.2.3",
		},
	}
	executable, err := executableForComponent(context.Background(), component)
	if err != nil || executable.SHA256 != digest {
		t.Fatalf("executable=%+v err=%v", executable, err)
	}
	t.Setenv("ENVTIE_ADAPTER_TRUST_KEYS", `{}`)
	if _, err := executableForComponent(context.Background(), component); err == nil || !strings.Contains(err.Error(), "adapter_registry_untrusted") {
		t.Fatalf("untrusted publisher: %v", err)
	}
}
