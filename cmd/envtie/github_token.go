package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coddicat/envtie-core/sdk/adapter"
)

const githubAppKeyEnv = "ENVTIE_GITHUB_APP_PRIVATE_KEY_FILE"

var githubAppClient = &http.Client{Timeout: 20 * time.Second}

// mintedGitHubToken is the installation token this process created. A token
// the operator exported is left unchanged.
var mintedGitHubToken struct {
	value   string
	expires time.Time
}

// supplyGitHubToken creates a short-lived installation token from the App
// private key when a local or Docker runner does not already have one.
// GitHub Actions supplies GITHUB_TOKEN in the workflow and is left unchanged.
func supplyGitHubToken(ctx context.Context, job runnerJob, gaps []adapter.Input) []adapter.Input {
	if !gapNamed(gaps, "GITHUB_TOKEN") || os.Getenv("GITHUB_ACTIONS") == "true" {
		return gaps
	}
	if os.Getenv("GITHUB_TOKEN") != "" {
		return withoutGap(gaps, "GITHUB_TOKEN")
	}
	repository := planRepository(job.Plan.Actions)
	if repository == "" {
		repository = strings.TrimSpace(os.Getenv("GITHUB_REPOSITORY"))
	}
	if repository == "" || job.GitHubAppID <= 0 {
		return gaps
	}
	path := strings.TrimSpace(os.Getenv(githubAppKeyEnv))
	if path == "" && runnerCanPrompt() {
		prompted, err := promptGitHubKeyPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, "runner:", err)
			return gaps
		}
		path = prompted
		_ = os.Setenv(githubAppKeyEnv, path)
	}
	if path == "" {
		return gaps
	}
	key, err := loadGitHubAppKey(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "runner:", err)
		return gaps
	}
	token, expires, err := mintInstallationToken(ctx, job.GitHubAppID, key, repository)
	if err != nil {
		fmt.Fprintln(os.Stderr, "runner:", err)
		return gaps
	}
	if err := os.Setenv("GITHUB_TOKEN", token); err != nil {
		return gaps
	}
	mintedGitHubToken.value = token
	mintedGitHubToken.expires = expires
	fmt.Fprintln(os.Stderr, "runner: created a GitHub App installation token for", repository)
	return withoutGap(gaps, "GITHUB_TOKEN")
}

func promptGitHubKeyPath() (string, error) {
	fmt.Fprintln(os.Stderr, "runner: enter the GitHub App private key file from Integrations. The key stays in this process, and the installation token is created from it.")
	for {
		fmt.Fprint(os.Stderr, "GitHub App private key file: ")
		value, err := readPromptLine(false)
		if err != nil {
			return "", err
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, err := loadGitHubAppKey(value); err != nil {
			fmt.Fprintln(os.Stderr, "runner:", err)
			continue
		}
		return value, nil
	}
}

func loadGitHubAppKey(path string) (*rsa.PrivateKey, error) {
	if path == "" || strings.ContainsAny(path, "\r\n") {
		return nil, errors.New("enter the path to the GitHub App private key")
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil, errors.New("that private key file was not found")
	}
	if info.Size() == 0 || info.Size() > 64*1024 {
		return nil, errors.New("that file is not a GitHub App private key")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("that private key file could not be read")
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("that file is not a GitHub App private key")
	}
	var parsed any
	switch block.Type {
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	default:
		return nil, errors.New("that file is not a GitHub App private key")
	}
	if err != nil {
		return nil, errors.New("that file is not a GitHub App private key")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("that file is not a GitHub App private key")
	}
	return key, nil
}

func mintInstallationToken(ctx context.Context, appID int64, key *rsa.PrivateKey, repository string) (string, time.Time, error) {
	owner, repo, ok := splitOwnerRepo(repository)
	if !ok || appID <= 0 {
		return "", time.Time{}, errors.New("the GitHub repository or App id is missing")
	}
	jwt, err := githubAppJWT(appID, key, time.Now())
	if err != nil {
		return "", time.Time{}, errors.New("the GitHub App private key could not be used")
	}
	var installation struct {
		ID int64 `json:"id"`
	}
	endpoint := githubAPI + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/installation"
	if status, err := githubAppJSON(ctx, http.MethodGet, endpoint, jwt, &installation); err != nil || installation.ID <= 0 {
		if status == http.StatusNotFound {
			return "", time.Time{}, fmt.Errorf("the GitHub App is not installed on %s", repository)
		}
		return "", time.Time{}, errors.New("the GitHub App installation could not be found")
	}
	var created struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	tokenURL := fmt.Sprintf("%s/app/installations/%d/access_tokens", githubAPI, installation.ID)
	if _, err := githubAppJSON(ctx, http.MethodPost, tokenURL, jwt, &created); err != nil || created.Token == "" {
		return "", time.Time{}, errors.New("GitHub did not create an installation token")
	}
	if created.ExpiresAt.IsZero() {
		created.ExpiresAt = time.Now().Add(50 * time.Minute)
	}
	return created.Token, created.ExpiresAt, nil
}

func githubAppJWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(8 * time.Minute).Unix(),
		"iss": fmt.Sprintf("%d", appID),
	})
	if err != nil {
		return "", err
	}
	body := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(body))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return body + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func githubAppJSON(ctx context.Context, method, endpoint, jwt string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := githubAppClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("github HTTP %d", resp.StatusCode)
	}
	if out == nil || len(raw) == 0 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, json.Unmarshal(raw, out)
}

func splitOwnerRepo(repository string) (string, string, bool) {
	owner, repo, ok := strings.Cut(strings.TrimSpace(repository), "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", false
	}
	return owner, repo, true
}

func gapNamed(gaps []adapter.Input, name string) bool {
	for _, gap := range gaps {
		if gap.Name == name {
			return true
		}
	}
	return false
}

func withoutGap(gaps []adapter.Input, name string) []adapter.Input {
	out := gaps[:0:0]
	for _, gap := range gaps {
		if gap.Name != name {
			out = append(out, gap)
		}
	}
	return out
}

func expireMintedGitHubToken() {
	if mintedGitHubToken.value == "" || time.Now().Before(mintedGitHubToken.expires.Add(-2*time.Minute)) {
		return
	}
	if os.Getenv("GITHUB_TOKEN") == mintedGitHubToken.value {
		_ = os.Unsetenv("GITHUB_TOKEN")
	}
	mintedGitHubToken.value = ""
}

func explainGitHubToken(job runnerJob) {
	repository := planRepository(job.Plan.Actions)
	switch {
	case job.GitHubAppID <= 0:
		fmt.Fprintln(os.Stderr, "runner: register the GitHub App under Integrations before a local runner can create an installation token")
	case repository == "" && strings.TrimSpace(os.Getenv("GITHUB_REPOSITORY")) == "":
		fmt.Fprintln(os.Stderr, "runner: the blueprint needs the repository on the GitHub provider before an installation token can be created")
	default:
		fmt.Fprintf(os.Stderr, "runner: set %s to the GitHub App private key from Integrations\n", githubAppKeyEnv)
	}
}
