package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coddicat/envtie-core/core"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

func TestMintInstallationTokenUsesTheBlueprintRepository(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var sawJWT bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(jwt, ".")
		if len(parts) != 3 {
			t.Errorf("authorization %q", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		raw, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Error(err)
		}
		var claims map[string]any
		if json.Unmarshal(raw, &claims) != nil || claims["iss"] != "42" {
			t.Errorf("claims %s", raw)
		}
		sawJWT = true
		switch r.URL.Path {
		case "/repos/acme/app/installation":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 99})
		case "/app/installations/99/access_tokens":
			if r.Method != http.MethodPost {
				t.Errorf("method %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "ghs_created", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	oldAPI := githubAPI
	oldClient := githubAppClient
	t.Cleanup(func() {
		githubAPI = oldAPI
		githubAppClient = oldClient
	})
	githubAPI = srv.URL
	githubAppClient = srv.Client()
	token, expires, err := mintInstallationToken(context.Background(), 42, key, "acme/app")
	if err != nil || token != "ghs_created" || !expires.After(time.Now()) || !sawJWT {
		t.Fatalf("token %q expires %s err %v jwt %v", token, expires, err, sawJWT)
	}
	if _, _, err := mintInstallationToken(context.Background(), 42, key, "acme/missing"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatal(err)
	}
}

func TestSupplyGitHubTokenReadsThePrivateKeyFile(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	path := filepath.Join(t.TempDir(), "app.private-key.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/shop/installation" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "ghs_from_file", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	}))
	defer srv.Close()
	oldAPI := githubAPI
	oldClient := githubAppClient
	t.Cleanup(func() {
		githubAPI = oldAPI
		githubAppClient = oldClient
		mintedGitHubToken.value = ""
	})
	githubAPI = srv.URL
	githubAppClient = srv.Client()
	mintedGitHubToken.value = ""
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv(githubAppKeyEnv, path)
	job := runnerJob{GitHubAppID: 42, Plan: core.Plan{Actions: []core.Action{{Component: core.Component{Config: map[string]string{"repository": "acme/shop"}}}}}}
	gaps := supplyGitHubToken(context.Background(), job, []adapter.Input{{Name: "GITHUB_TOKEN", Kind: adapter.InputSecret, Required: true}})
	if len(gaps) != 0 || os.Getenv("GITHUB_TOKEN") != "ghs_from_file" {
		t.Fatalf("gaps %+v token %q", gaps, os.Getenv("GITHUB_TOKEN"))
	}
}
