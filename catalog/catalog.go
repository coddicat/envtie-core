// Package adapters loads the official adapter catalog published with a release.
// The catalog is the trust source for Neon, Upstash, Stripe, and GitHub adapter
// artifacts. Runners reject a binary whose SHA-256 does not match its signed plan.
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/coddicat/envtie-core/core"
)

var sha256Hex = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
var targetName = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+$`)
var releaseArtifact = regexp.MustCompile(`^adapter-(neon|upstash|github|stripe)_(.+)_(linux|darwin|windows)_(amd64|arm64)(\.exe)?$`)

// Official reports whether EnvTie.dev publishes this adapter and can vouch for its pin.
func Official(adapter string) bool {
	switch adapter {
	case "neon@1", "upstash@1", "stripe@1", "github@1":
		return true
	default:
		return false
	}
}

// Pin identifies one immutable official adapter build.
type Pin struct {
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
	Size   int64  `json:"size,omitempty"`
}

// Catalog maps an adapter id such as neon@1 to OS/architecture pins.
type Catalog struct {
	Release  string                    `json:"release"`
	BaseURL  string                    `json:"base_url,omitempty"`
	Adapters map[string]map[string]Pin `json:"adapters"`
}

// Load reads a catalog file. A missing file is reported to the caller.
func Load(path string) (Catalog, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Catalog{}, err
	}
	return Parse(body)
}

// Parse validates a catalog document.
func Parse(body []byte) (Catalog, error) {
	var catalog Catalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		return Catalog{}, fmt.Errorf("adapter catalog: %w", err)
	}
	if catalog.Release == "" || len(catalog.Release) > 80 {
		return Catalog{}, errors.New("adapter catalog needs a release name")
	}
	if catalog.Adapters == nil {
		return Catalog{}, errors.New("adapter catalog needs an adapters object")
	}
	for adapter, targets := range catalog.Adapters {
		if !Official(adapter) {
			return Catalog{}, fmt.Errorf("adapter catalog lists unknown adapter %s", adapter)
		}
		if len(targets) == 0 {
			return Catalog{}, fmt.Errorf("adapter catalog has no builds for %s", adapter)
		}
		for target, pin := range targets {
			if !targetName.MatchString(target) || !sha256Hex.MatchString(pin.SHA256) || pin.Size < 0 {
				return Catalog{}, fmt.Errorf("adapter catalog has an invalid pin for %s on %s", adapter, target)
			}
			if pin.URL == "" && catalog.BaseURL == "" {
				return Catalog{}, fmt.Errorf("adapter catalog has no artifact URL for %s on %s", adapter, target)
			}
		}
	}
	return catalog, nil
}

// Resolve returns all platform artifacts for an official adapter. These values
// are copied into the blueprint version and therefore into the signed plan.
func (c Catalog) Resolve(adapter string) (map[string]core.AdapterArtifact, error) {
	targets, ok := c.Adapters[adapter]
	if !ok {
		return nil, fmt.Errorf("official adapter catalog has no %s release", adapter)
	}
	result := make(map[string]core.AdapterArtifact, len(targets))
	for target, pin := range targets {
		url := pin.URL
		if url == "" {
			url = strings.TrimRight(c.BaseURL, "/") + "/" + ArtifactFilename(adapter, c.Release, target)
		}
		result[target] = core.AdapterArtifact{
			Release: c.Release,
			URL:     url,
			SHA256:  strings.ToLower(pin.SHA256),
			Size:    pin.Size,
		}
	}
	return result, nil
}

// Build catalogs the release-named adapter binaries in dir. Their URLs point at
// the control plane's own artifact route, so a hosted control plane can serve
// the adapters built from the same commit.
func Build(dir, release string) (Catalog, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Catalog{}, err
	}
	catalog := Catalog{Release: release, Adapters: map[string]map[string]Pin{}}
	for _, entry := range entries {
		match := releaseArtifact.FindStringSubmatch(entry.Name())
		if match == nil || match[2] != release || !entry.Type().IsRegular() {
			continue
		}
		adapter, target := match[1]+"@1", match[3]+"/"+match[4]
		if entry.Name() != ArtifactFilename(adapter, release, target) {
			continue
		}
		sum, size, err := fileDigest(filepath.Join(dir, entry.Name()))
		if err != nil {
			return Catalog{}, err
		}
		if catalog.Adapters[adapter] == nil {
			catalog.Adapters[adapter] = map[string]Pin{}
		}
		catalog.Adapters[adapter][target] = Pin{SHA256: sum, Size: size, URL: "/v1/adapter-artifacts/" + adapter + "/" + target}
	}
	if len(catalog.Adapters) == 0 {
		return Catalog{}, fmt.Errorf("no %s adapter builds in %s", release, dir)
	}
	body, err := json.Marshal(catalog)
	if err != nil {
		return Catalog{}, err
	}
	return Parse(body)
}

func fileDigest(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

// ArtifactFilename is the release asset name of one official adapter build.
func ArtifactFilename(adapter, release, target string) string {
	name := strings.TrimSuffix(adapter, "@1")
	parts := strings.Split(target, "/")
	ext := ""
	if len(parts) == 2 && parts[0] == "windows" {
		ext = ".exe"
	}
	return fmt.Sprintf("adapter-%s_%s_%s_%s%s", name, release, parts[0], parts[1], ext)
}

// Allows reports whether sum is one of the published pins for an official adapter.
func (c Catalog) Allows(adapter, sum string) bool {
	targets, ok := c.Adapters[adapter]
	if !ok {
		return false
	}
	for _, pin := range targets {
		if strings.EqualFold(pin.SHA256, sum) {
			return true
		}
	}
	return false
}

// Enforce rejects an official adapter pin that this catalog does not publish.
// An empty catalog leaves enforcement off so existing tests and custom-only
// installs keep working until a release catalog is configured.
func (c Catalog) Enforce(adapter, sum string) error {
	if !Official(adapter) || len(c.Adapters) == 0 {
		return nil
	}
	if c.Allows(adapter, sum) {
		return nil
	}
	return fmt.Errorf("%s must use a SHA-256 pin from the official adapter catalog", adapter)
}
