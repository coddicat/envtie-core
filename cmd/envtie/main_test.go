package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coddicat/envtie-core/core"
	plansig "github.com/coddicat/envtie-core/plan"
	"github.com/coddicat/envtie-core/sdk/adapter"
	"golang.org/x/crypto/nacl/box"
)

func TestPreviewActionArgsAcceptRunOnEitherSide(t *testing.T) {
	name, run, err := previewActionArgs("apply", []string{"pr-15", "--run"})
	if err != nil || name != "pr-15" || !run {
		t.Fatalf("flag last: %q %v %v", name, run, err)
	}
	name, run, err = previewActionArgs("close", []string{"--run", "pr-15"})
	if err != nil || name != "pr-15" || !run {
		t.Fatalf("flag first: %q %v %v", name, run, err)
	}
	if _, _, err = previewActionArgs("apply", []string{"pr-15", "extra"}); err == nil {
		t.Fatal("extra argument accepted")
	}
}

func TestOutputsMaskSecretInternalAndUnknownFields(t *testing.T) {
	b := core.Blueprint{Components: []core.Component{{ID: "db", Adapter: "fake@1", Ownership: core.Managed}}}
	env := core.Environment{Resources: map[string]core.Resource{"db": {ExternalID: "safe-id", Adapter: "fake@1", Ownership: core.Managed}}}
	call := func(_ context.Context, _ core.Component, r adapter.Request) (adapter.Response, error) {
		if r.Hook == "manifest" {
			return adapter.Response{Manifest: &adapter.Manifest{Name: "fake", Version: "1.0.0", Protocol: 1, Outputs: []adapter.Field{
				{Name: "URL", Sensitivity: "SECRET"}, {Name: "ACCOUNT", Sensitivity: "INTERNAL"}, {Name: "ID", Sensitivity: "PUBLIC"},
			}}}, nil
		}
		return adapter.Response{Outputs: map[string]string{"URL": "secret-fixture", "ACCOUNT": "internal-fixture", "UNKNOWN": "unknown-fixture", "ID": "safe-id"}}, nil
	}
	masked, err := outputValues(context.Background(), b, env, false, call)
	if err != nil || masked["db.URL"] != "[REDACTED]" || masked["db.ACCOUNT"] != "[REDACTED]" || masked["db.UNKNOWN"] != "[REDACTED]" || masked["db.ID"] != "safe-id" {
		t.Fatal("outputs must mask non-public fields")
	}
	revealed, err := outputValues(context.Background(), b, env, true, call)
	if err != nil || revealed["db.URL"] != "secret-fixture" {
		t.Fatal("explicit local reveal must retrieve outputs")
	}
}

func TestSecretRevealRejectedBeforeProviderAccessInCI(t *testing.T) {
	for _, variable := range []string{"CI", "GITHUB_ACTIONS", "GITHUB_RUN_ID"} {
		t.Run(variable, func(t *testing.T) {
			t.Setenv(variable, "true")
			if err := outputs([]string{"--reveal", "preview"}); err == nil || !strings.Contains(err.Error(), "disabled in CI") {
				t.Fatal("CI reveal must fail before loading state or calling providers")
			}
		})
	}
}

func TestDestroyUsesApprovedSnapshotAfterYAMLChanges(t *testing.T) {
	t.Chdir(t.TempDir())
	b := core.Blueprint{Name: "preview", Components: []core.Component{{ID: "shared", Adapter: "fake@1", Executable: "fake", SHA256: strings.Repeat("a", 64), Ownership: core.Referenced, Config: map[string]string{"id": "safe-id"}}}}
	env := core.Environment{Name: "preview", Resources: map[string]core.Resource{"shared": {ComponentID: "shared", Adapter: "fake@1", Ownership: core.Referenced, ExternalID: "safe-id"}}}
	if err := saveJSON(envPath(env.Name), env); err != nil {
		t.Fatal(err)
	}
	if err := saveJSON(filepath.Join(stateDir(), "blueprint-env-preview.json"), b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("envtie.yaml", []byte("invalid changed yaml"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := destroy([]string{"preview", "--blueprint", "envtie.yaml"}); err == nil {
		t.Fatal("explicit mismatched blueprint accepted")
	}
	if err := destroy([]string{"preview"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadEnv("preview")
	if err != nil || loaded.State != "DESTROYED" {
		t.Fatal("approved snapshot was not used")
	}
}

func TestSecureAPIURLAllowsHTTPOnlyOnLoopback(t *testing.T) {
	for url, want := range map[string]bool{
		"https://envtie.example.com":        true,
		"http://localhost:8080":             true,
		"http://127.0.0.1:8080":             true,
		"http://[::1]:8080":                 true,
		"http://envtie.example.com":         false,
		"http://localhost.example.com:8080": false,
		"http://keycloak:8080":              false,
		"https://user@envtie.example.com":   false,
		"localhost:8080":                    false,
		"":                                  false,
	} {
		if got := secureAPIURL(url); got != want {
			t.Errorf("secureAPIURL(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestAutomationRequestUsesScopedTokenAndReturnsSafeErrors(t *testing.T) {
	var authorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		if strings.Contains(r.URL.Path, "failure") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"protected_environment"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(previewOperation{OperationID: "00000000-0000-0000-0000-000000000001", Status: "queued"})
	}))
	defer srv.Close()
	t.Setenv("ENVTIE_API_URL", srv.URL)
	t.Setenv("ENVTIE_CI_TOKEN", "etci_secret-fixture")
	var result previewOperation
	if err := automationRequest(http.MethodPost, "/v1/custom-ci/environments/pr-1/apply", &result); err != nil {
		t.Fatal(err)
	}
	if authorization != "Bearer etci_secret-fixture" || result.Status != "queued" {
		t.Fatal("automation request did not authenticate or decode response")
	}
	if err := automationRequest(http.MethodPost, "/failure", &result); err == nil || err.Error() != "protected_environment" || strings.Contains(err.Error(), "secret-fixture") {
		t.Fatalf("unexpected safe API error: %v", err)
	}
}

type fakeControlPlane struct {
	mu       sync.Mutex
	jobs     []any
	status   int
	reports  []bool
	leases   int
	releases int
}

func (f *fakeControlPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer runner-token" {
		w.WriteHeader(401)
		return
	}
	switch {
	case r.URL.Path == "/v1/runner/jobs":
		f.leases++
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		if len(f.jobs) == 0 {
			w.WriteHeader(204)
			return
		}
		job := f.jobs[0]
		f.jobs = f.jobs[1:]
		_ = json.NewEncoder(w).Encode(job)
	case strings.HasSuffix(r.URL.Path, "/checkpoint"):
		w.WriteHeader(204)
	case strings.HasSuffix(r.URL.Path, "/release"):
		f.releases++
		w.WriteHeader(204)
	case strings.HasSuffix(r.URL.Path, "/report"):
		var report struct {
			Success bool `json:"success"`
		}
		_ = json.NewDecoder(r.Body).Decode(&report)
		f.reports = append(f.reports, report.Success)
		w.WriteHeader(204)
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeControlPlane) snapshot() (int, []bool, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leases, append([]bool(nil), f.reports...), f.releases
}

func testRunner(t *testing.T, cp *fakeControlPlane) (runnerConfig, ed25519.PrivateKey) {
	t.Helper()
	srv := httptest.NewServer(cp)
	t.Cleanup(srv.Close)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	call := func(_ context.Context, _ core.Component, r adapter.Request) (adapter.Response, error) {
		switch r.Hook {
		case "manifest":
			return adapter.Response{Manifest: &adapter.Manifest{Name: "fake", Version: "1.0.0", Protocol: 1, Capabilities: []string{"read", "apply", "validate", "outputs", "destroy"}}}, nil
		case "read":
			return adapter.Response{Absent: true}, nil
		case "apply":
			return adapter.Response{ExternalID: "res-1", Status: "created"}, nil
		case "validate":
			return adapter.Response{Status: "ready"}, nil
		case "outputs":
			return adapter.Response{}, nil
		}
		return adapter.Response{}, errors.New("unexpected hook")
	}
	return runnerConfig{base: srv.URL, token: "runner-token", pub: pub, client: srv.Client(), once: true, call: call, idle: time.Millisecond}, priv
}

func signedJob(t *testing.T, priv ed25519.PrivateKey, env string) runnerJob {
	t.Helper()
	b := core.Blueprint{Name: "preview", Components: []core.Component{{ID: "svc", Adapter: "fake@1", Executable: "fake", SHA256: strings.Repeat("a", 64), Ownership: core.Managed}}}
	p, err := core.NewPlan(b, env, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := plansig.Sign(&p, "ws-1", priv); err != nil {
		t.Fatal(err)
	}
	return runnerJob{OperationID: "op-" + env, WorkspaceID: "ws-1", Kind: "apply", Plan: p, Environment: core.Environment{Name: env, State: "PLANNED", Resources: map[string]core.Resource{}}}
}

func TestRunnerOnceExitsWhenNoWork(t *testing.T) {
	cp := &fakeControlPlane{}
	cfg, _ := testRunner(t, cp)
	if err := executeRunner(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if leases, _, _ := cp.snapshot(); leases != 1 {
		t.Fatalf("leases=%d, want exactly one check", leases)
	}
}

func TestRunnerOnceDrainsWaitingJobs(t *testing.T) {
	cp := &fakeControlPlane{}
	cfg, priv := testRunner(t, cp)
	cp.jobs = []any{signedJob(t, priv, "pr-1"), signedJob(t, priv, "pr-2")}
	if err := executeRunner(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, reports, _ := cp.snapshot(); len(reports) != 2 || !reports[0] || !reports[1] {
		t.Fatalf("reports=%v", reports)
	}
}

func TestRunnerOnceFailsOnRejectedPlan(t *testing.T) {
	cp := &fakeControlPlane{}
	cfg, _ := testRunner(t, cp)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	cp.jobs = []any{signedJob(t, otherKey, "pr-1")}
	err := executeRunner(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "1 of 1 job(s) failed") {
		t.Fatalf("err=%v", err)
	}
	if _, reports, _ := cp.snapshot(); len(reports) != 0 {
		t.Fatal("a plan with an invalid signature must never run or report")
	}
}

func TestRunnerOnceFailsFastOnBadTokenOrOutage(t *testing.T) {
	cp := &fakeControlPlane{}
	cfg, _ := testRunner(t, cp)
	cfg.token = "wrong"
	if err := executeRunner(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "token rejected") {
		t.Fatalf("bad token: %v", err)
	}
	cfg.token = "runner-token"
	cp.status = 503
	if err := executeRunner(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "control plane unavailable") {
		t.Fatalf("outage: %v", err)
	}
}

func TestRunnerWatchModeStopsWhenTokenRejected(t *testing.T) {
	cp := &fakeControlPlane{status: 401}
	cfg, _ := testRunner(t, cp)
	cfg.once = false
	err := executeRunner(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "token rejected") {
		t.Fatalf("err=%v", err)
	}
	if leases, _, _ := cp.snapshot(); leases != 1 {
		t.Fatalf("rejected token retried, leases=%d", leases)
	}
}

func TestRunnerWatchModeStopsOnCancel(t *testing.T) {
	cp := &fakeControlPlane{}
	cfg, _ := testRunner(t, cp)
	cfg.once = false
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := executeRunner(ctx, cfg); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if leases, _, _ := cp.snapshot(); leases < 2 {
		t.Fatalf("watch mode should keep polling, leases=%d", leases)
	}
}

func TestRunnerStopReleasesLeaseWithoutReporting(t *testing.T) {
	cp := &fakeControlPlane{}
	cfg, priv := testRunner(t, cp)
	cfg.once = false
	cp.jobs = []any{signedJob(t, priv, "held")}
	started := make(chan struct{})
	cfg.call = func(ctx context.Context, _ core.Component, r adapter.Request) (adapter.Response, error) {
		switch r.Hook {
		case "manifest":
			return adapter.Response{Manifest: &adapter.Manifest{Name: "fake", Version: "1.0.0", Protocol: 1, Capabilities: []string{"read", "apply", "validate", "outputs", "destroy"}}}, nil
		case "read":
			return adapter.Response{Absent: true}, nil
		case "apply":
			close(started)
			<-ctx.Done()
			return adapter.Response{}, ctx.Err()
		default:
			return adapter.Response{}, errors.New("unexpected hook")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- executeRunner(ctx, cfg) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("apply did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not stop")
	}
	if _, reports, releases := cp.snapshot(); len(reports) != 0 || releases != 1 {
		t.Fatalf("reports=%v releases=%d", reports, releases)
	}
}

func TestPreviewRejectsRetiredHostedCommands(t *testing.T) {
	for _, args := range [][]string{
		{"execute"},
		{"execute", "--dispatch", "22222222-2222-2222-2222-222222222222"},
		{"report"},
		{"report", "--dispatch", "22222222-2222-2222-2222-222222222222", "--status", "failure"},
	} {
		err := preview(args)
		if err == nil {
			t.Fatalf("preview %v succeeded", args)
		}
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "dispatch") || strings.Contains(message, "oidc") || strings.Contains(message, "actions") {
			t.Fatalf("preview %v still describes the hosted path: %v", args, err)
		}
		if !strings.Contains(err.Error(), "preview apply") {
			t.Fatalf("preview %v: %v", args, err)
		}
	}
}

func TestPreviewCommandsUseCustomCI(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer et_project_key" {
			t.Errorf("authorization %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/v1/custom-ci/sweep":
			_ = json.NewEncoder(w).Encode(map[string]any{"environments": []string{"pr-9"}})
		case "/v1/custom-ci/operations/op-1":
			_ = json.NewEncoder(w).Encode(previewOperation{OperationID: "op-1", Status: "completed"})
		default:
			_ = json.NewEncoder(w).Encode(previewOperation{OperationID: "op-1", Status: "queued"})
		}
	}))
	defer srv.Close()
	t.Setenv("ENVTIE_API_URL", srv.URL)
	t.Setenv("ENVTIE_API_KEY", "et_project_key")
	t.Setenv("ENVTIE_CI_TOKEN", "")
	for _, args := range [][]string{
		{"apply", "pr-4"},
		{"close", "pr-4"},
		{"destroy", "pr-4"},
		{"sweep"},
		{"status", "op-1"},
	} {
		if err := preview(args); err != nil {
			t.Fatalf("preview %v: %v", args, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(paths, "\n")
	for _, needle := range []string{
		"POST /v1/custom-ci/environments/pr-4/apply",
		"POST /v1/custom-ci/environments/pr-4/close",
		"POST /v1/custom-ci/environments/pr-4/destroy",
		"POST /v1/custom-ci/sweep",
		"GET /v1/custom-ci/operations/op-1",
	} {
		if !strings.Contains(joined, needle) {
			t.Fatalf("missing %s in\n%s", needle, joined)
		}
	}
	for _, retired := range []string{"/v1/ci/", "/v1/github/actions", "/v1/runner/jobs"} {
		if strings.Contains(joined, retired) {
			t.Fatalf("preview commands used %s\n%s", retired, joined)
		}
	}
}

func TestPreviewApplyRunLeasesCustomCIOperation(t *testing.T) {
	const operationID = "11111111-1111-1111-1111-111111111111"
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		switch r.URL.Path {
		case "/v1/custom-ci/environments/pr-4/apply":
			if r.URL.RawQuery != "execute=1" {
				t.Errorf("query %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(previewOperation{OperationID: operationID, Status: "queued"})
		case "/v1/custom-ci/operations/" + operationID + "/lease":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("ENVTIE_API_URL", srv.URL)
	t.Setenv("ENVTIE_API_KEY", "et_project_key")
	t.Setenv("ENVTIE_CI_TOKEN", "")
	err := preview([]string{"apply", "--run", "pr-4"})
	if err == nil || err.Error() != "no job" {
		t.Fatalf("err=%v", err)
	}
	joined := strings.Join(paths, "\n")
	for _, needle := range []string{
		"POST /v1/custom-ci/environments/pr-4/apply?execute=1",
		"POST /v1/custom-ci/operations/" + operationID + "/lease",
	} {
		if !strings.Contains(joined, needle) {
			t.Fatalf("missing %s in\n%s", needle, joined)
		}
	}
}

func TestCustomCILeaseCheckpointAndReport(t *testing.T) {
	const operationID = "11111111-1111-1111-1111-111111111111"
	var mu sync.Mutex
	var paths []string
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	job := signedJob(t, priv, "pr-1")
	job.OperationID = operationID
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer ci-token" {
			t.Errorf("authorization %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/v1/custom-ci/operations/" + operationID + "/lease":
			_ = json.NewEncoder(w).Encode(struct {
				runnerJob
				PublicKey string `json:"public_key"`
			}{runnerJob: job, PublicKey: base64.StdEncoding.EncodeToString(pub)})
		case "/v1/custom-ci/operations/" + operationID + "/checkpoint",
			"/v1/custom-ci/operations/" + operationID + "/report":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	cfg := runnerConfig{
		base: srv.URL, token: "ci-token", client: srv.Client(),
		jobsRoot: "/v1/custom-ci/operations", exactOperation: operationID,
		call: func(_ context.Context, _ core.Component, req adapter.Request) (adapter.Response, error) {
			switch req.Hook {
			case "manifest":
				return adapter.Response{Manifest: &adapter.Manifest{Name: "fake", Version: "1.0.0", Protocol: 1, Capabilities: []string{"read", "apply", "validate", "outputs", "destroy"}}}, nil
			case "read":
				return adapter.Response{Absent: true}, nil
			case "apply":
				return adapter.Response{ExternalID: "res-1", Status: "created"}, nil
			case "validate":
				return adapter.Response{Status: "ready"}, nil
			case "outputs":
				return adapter.Response{}, nil
			default:
				return adapter.Response{}, errors.New("unexpected hook")
			}
		},
	}
	leased, err := cfg.lease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if leased.OperationID != operationID {
		t.Fatalf("leased %s", leased.OperationID)
	}
	if err := cfg.runJob(context.Background(), leased); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(paths, "\n")
	for _, needle := range []string{
		"POST /v1/custom-ci/operations/" + operationID + "/lease",
		"POST /v1/custom-ci/operations/" + operationID + "/checkpoint",
		"POST /v1/custom-ci/operations/" + operationID + "/report",
	} {
		if !strings.Contains(joined, needle) {
			t.Fatalf("missing %s in\n%s", needle, joined)
		}
	}
	for _, retired := range []string{"/v1/ci/", "/v1/github/actions", "/v1/runner/jobs"} {
		if strings.Contains(joined, retired) {
			t.Fatalf("custom CI job used %s\n%s", retired, joined)
		}
	}
}

func TestGitHubEnvironmentSecretIsSealedForGitHub(t *testing.T) {
	publicKey, privateKey, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "database-secret-fixture"
	decrypted := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer installation-token" {
			t.Error("missing installation token")
		}
		switch r.URL.Path {
		case "/repos/acme/app/environments/pr-7":
			if r.Method != http.MethodGet {
				t.Errorf("delivery created an environment with %s", r.Method)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"name": "pr-7"})
		case "/repos/acme/app/environments/pr-7/variables":
			_ = json.NewEncoder(w).Encode(map[string]any{"variables": []map[string]string{{"name": "ENVTIE_MANAGED", "value": "true"}}})
		case "/repos/acme/app/environments/pr-7/secrets/public-key":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"key_id": "key-1", "key": base64.StdEncoding.EncodeToString(publicKey[:]),
			})
		case "/repos/acme/app/environments/pr-7/secrets/DATABASE_URL":
			var in struct {
				KeyID          string `json:"key_id"`
				EncryptedValue string `json:"encrypted_value"`
			}
			if json.NewDecoder(r.Body).Decode(&in) != nil || in.KeyID != "key-1" || strings.Contains(in.EncryptedValue, secret) {
				t.Error("invalid encrypted delivery")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			ciphertext, decodeErr := base64.StdEncoding.DecodeString(in.EncryptedValue)
			if decodeErr != nil {
				t.Error(decodeErr)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			plain, ok := box.OpenAnonymous(nil, ciphertext, publicKey, privateKey)
			if !ok {
				t.Error("ciphertext could not be opened")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			decrypted = string(plain)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("GITHUB_TOKEN", "installation-token")
	t.Setenv("GITHUB_REPOSITORY", "")
	githubAPI = srv.URL
	t.Cleanup(func() { githubAPI = "https://api.github.com" })
	cfg := runnerConfig{}
	if err := cfg.deliverGitHubEnvironment(context.Background(), "pr-7", "github.environment.secret.DATABASE_URL", secret, "acme/app"); err != nil {
		t.Fatal(err)
	}
	if decrypted != secret {
		t.Fatalf("decrypted value %q", decrypted)
	}
}

func TestGitHubEnvironmentDeliveryDoesNotCreate(t *testing.T) {
	created := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			created = true
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("GITHUB_TOKEN", "installation-token")
	t.Setenv("GITHUB_REPOSITORY", "acme/app")
	githubAPI = srv.URL
	t.Cleanup(func() { githubAPI = "https://api.github.com" })
	err := (runnerConfig{}).deliverGitHubEnvironment(context.Background(), "pr-7", "github.environment.secret.DATABASE_URL", "secret", "")
	if err == nil || created {
		t.Fatalf("missing environment was created: err=%v created=%v", err, created)
	}
}

func TestGitHubForkDeliveryFails(t *testing.T) {
	t.Setenv("ENVTIE_FORK", "true")
	t.Setenv("GITHUB_TOKEN", "installation-token")
	t.Setenv("GITHUB_REPOSITORY", "acme/app")
	err := (runnerConfig{}).deliverGitHubEnvironment(context.Background(), "pr-7", "github.environment.secret.DATABASE_URL", "secret", "")
	if err == nil || err.Error() != "github_fork_secrets_unavailable" {
		t.Fatal(err)
	}
}

func TestWorkflowPullRequestMatchesEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "event.json")
	event := `{"pull_request":{"number":12,"title":"Add checkout","state":"open","html_url":"https://github.com/acme/shop/pull/12","user":{"login":"dev"},"head":{"ref":"feature/checkout","sha":"abc"}}}`
	if err := os.WriteFile(path, []byte(event), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_EVENT_PATH", path)
	t.Setenv("GITHUB_REPOSITORY", "acme/shop")
	pr := workflowPullRequest("pr-12")
	if pr == nil || pr["number"] != 12 || pr["branch"] != "feature/checkout" || pr["repository"] != "acme/shop" {
		t.Fatalf("%#v", pr)
	}
	if workflowPullRequest("pr-13") != nil {
		t.Fatal("metadata was sent for another environment")
	}
}

func TestEnsureActionsGitContextSeedsMissingVars(t *testing.T) {
	t.Setenv("GITHUB_HEAD_REF", "")
	t.Setenv("GITHUB_SHA", "")
	ensureActionsGitContext(map[string]any{"branch": "feature/checkout", "sha": "abc123"})
	if got := os.Getenv("GITHUB_HEAD_REF"); got != "feature/checkout" {
		t.Fatalf("GITHUB_HEAD_REF=%q", got)
	}
	if got := os.Getenv("GITHUB_SHA"); got != "abc123" {
		t.Fatalf("GITHUB_SHA=%q", got)
	}
	t.Setenv("GITHUB_HEAD_REF", "already-set")
	t.Setenv("GITHUB_SHA", "already-sha")
	ensureActionsGitContext(map[string]any{"branch": "other", "sha": "other-sha"})
	if os.Getenv("GITHUB_HEAD_REF") != "already-set" || os.Getenv("GITHUB_SHA") != "already-sha" {
		t.Fatal("existing Actions context was overwritten")
	}
}
