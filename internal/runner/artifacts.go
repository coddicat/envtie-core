package runner

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coddicat/envtie-core/core"
	"github.com/coddicat/envtie-core/internal/envx"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

const maxAdapterBytes = 128 << 20

var artifactLocks sync.Map

func executableForComponent(ctx context.Context, c core.Component) (adapter.Executable, error) {
	if len(c.Artifacts) == 0 {
		if c.Source != nil {
			artifacts, err := resolveCustomArtifacts(ctx, c.Adapter, *c.Source)
			if err != nil {
				return adapter.Executable{}, err
			}
			c.Artifacts = artifacts
		} else {
			return adapter.Executable{Path: c.Executable, SHA256: c.SHA256}, nil
		}
	}
	if len(c.Artifacts) == 0 {
		return adapter.Executable{Path: c.Executable, SHA256: c.SHA256}, nil
	}
	target := runtime.GOOS + "/" + runtime.GOARCH
	artifact, ok := c.Artifacts[target]
	if !ok {
		return adapter.Executable{}, adapter.NewError("adapter_platform_unavailable")
	}
	path, err := installArtifact(ctx, c.Adapter, target, artifact)
	if err != nil {
		var coded *adapter.CodedError
		if errors.As(err, &coded) {
			return adapter.Executable{}, err
		}
		return adapter.Executable{}, adapter.NewError("adapter_install_failed")
	}
	return adapter.Executable{Path: path, SHA256: artifact.SHA256}, nil
}

type customRegistryRelease struct {
	Adapter   string                          `json:"adapter"`
	Publisher string                          `json:"publisher"`
	Version   string                          `json:"version"`
	Artifacts map[string]core.AdapterArtifact `json:"artifacts"`
}

type customRegistryEnvelope struct {
	Signed    customRegistryRelease `json:"signed"`
	Signature string                `json:"signature"`
}

func resolveCustomArtifacts(ctx context.Context, adapterID string, source core.AdapterSource) (map[string]core.AdapterArtifact, error) {
	base, err := artifactURL(source.Registry)
	if err != nil {
		return nil, adapter.NewError("adapter_registry_untrusted")
	}
	endpoint := strings.TrimRight(base, "/") + "/v1/adapters/" + url.PathEscape(adapterID) + "/" + url.PathEscape(source.Version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, adapter.NewError("adapter_install_failed")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, adapter.NewError("adapter_install_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, adapter.NewError("adapter_install_failed")
	}
	var envelope customRegistryEnvelope
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	dec.DisallowUnknownFields()
	if dec.Decode(&envelope) != nil || envelope.Signed.Adapter != adapterID ||
		envelope.Signed.Publisher != source.Publisher || envelope.Signed.Version != source.Version ||
		len(envelope.Signed.Artifacts) == 0 {
		return nil, adapter.NewError("adapter_registry_untrusted")
	}
	keys, err := customTrustKeys()
	if err != nil {
		return nil, adapter.NewError("adapter_registry_untrusted")
	}
	key, ok := keys[source.Publisher]
	if !ok {
		return nil, adapter.NewError("adapter_registry_untrusted")
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil {
		return nil, adapter.NewError("adapter_registry_untrusted")
	}
	payload, _ := json.Marshal(envelope.Signed)
	if !ed25519.Verify(key, payload, signature) {
		return nil, adapter.NewError("adapter_registry_untrusted")
	}
	for target, artifact := range envelope.Signed.Artifacts {
		if target == "" || artifact.Release != source.Version || len(artifact.SHA256) != 64 || artifact.URL == "" || artifact.Size < 0 {
			return nil, adapter.NewError("adapter_registry_untrusted")
		}
	}
	return envelope.Signed.Artifacts, nil
}

func customTrustKeys() (map[string]ed25519.PublicKey, error) {
	raw := []byte(envx.Get("ADAPTER_TRUST_KEYS"))
	if path := envx.Get("ADAPTER_TRUST_FILE"); path != "" {
		var err error
		raw, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
	}
	if len(raw) == 0 {
		return nil, errors.New("custom adapter trust is not configured")
	}
	var encoded map[string]string
	if json.Unmarshal(raw, &encoded) != nil {
		return nil, errors.New("invalid custom adapter trust")
	}
	result := make(map[string]ed25519.PublicKey, len(encoded))
	for publisher, value := range encoded {
		key, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(key) != ed25519.PublicKeySize || safePathPart(publisher) != publisher {
			return nil, errors.New("invalid custom adapter trust")
		}
		result[publisher] = ed25519.PublicKey(key)
	}
	return result, nil
}

func installArtifact(ctx context.Context, adapterID, target string, artifact core.AdapterArtifact) (string, error) {
	if artifact.Release == "" || len(artifact.SHA256) != 64 || artifact.URL == "" {
		return "", errors.New("invalid adapter artifact")
	}
	cache, err := artifactCacheDir()
	if err != nil {
		return "", err
	}
	name := strings.TrimSuffix(adapterID, "@1")
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	dest := filepath.Join(cache, safePathPart(artifact.Release), strings.ReplaceAll(target, "/", "-"), name, artifact.SHA256, "adapter-"+name+ext)
	lockValue, _ := artifactLocks.LoadOrStore(dest, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	if adapter.VerifyChecksum(dest, artifact.SHA256) == nil {
		return dest, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return "", err
	}
	source, err := artifactURL(artifact.URL)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{
		Timeout: 90 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" && !localHTTP(req.URL) {
				return errors.New("insecure adapter redirect")
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("adapter download returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxAdapterBytes || artifact.Size > 0 && resp.ContentLength > 0 && resp.ContentLength != artifact.Size {
		return "", errors.New("adapter artifact size mismatch")
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(resp.Body, maxAdapterBytes+1))
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil || n > maxAdapterBytes || artifact.Size > 0 && n != artifact.Size {
		return "", errors.New("adapter artifact download incomplete")
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), artifact.SHA256) {
		return "", adapter.NewError("adapter_checksum_mismatch")
	}
	if err := os.Chmod(tmpName, 0700); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		if adapter.VerifyChecksum(dest, artifact.SHA256) == nil {
			return dest, nil
		}
		return "", err
	}
	return dest, nil
}

func artifactCacheDir() (string, error) {
	if path := envx.Get("ADAPTER_CACHE"); path != "" {
		return filepath.Abs(path)
	}
	root, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "envtie", "adapters"), nil
}

func artifactURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if !u.IsAbs() {
		base, err := url.Parse(envx.Get("API_URL"))
		if err != nil || !base.IsAbs() {
			return "", errors.New("relative adapter URL needs ENVTIE_API_URL")
		}
		u = base.ResolveReference(u)
	}
	if u.Scheme != "https" && !localHTTP(u) {
		return "", errors.New("adapter artifacts require HTTPS")
	}
	if u.User != nil || u.Fragment != "" {
		return "", errors.New("invalid adapter artifact URL")
	}
	return u.String(), nil
}

func localHTTP(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	return u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")
}

func safePathPart(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "release"
	}
	return b.String()
}
