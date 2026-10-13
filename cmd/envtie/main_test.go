package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
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
	mu      sync.Mutex
	jobs    []any
	status  int
	reports []bool
	leases  int
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

func (f *fakeControlPlane) snapshot() (int, []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.leases, append([]bool(nil), f.reports...)
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
	if leases, _ := cp.snapshot(); leases != 1 {
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
	if _, reports := cp.snapshot(); len(reports) != 2 || !reports[0] || !reports[1] {
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
	if _, reports := cp.snapshot(); len(reports) != 0 {
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

func TestRunnerWatchModeStopsOnCancel(t *testing.T) {
	cp := &fakeControlPlane{}
	cfg, _ := testRunner(t, cp)
	cfg.once = false
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := executeRunner(ctx, cfg); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if leases, _ := cp.snapshot(); leases < 2 {
		t.Fatalf("watch mode should keep polling, leases=%d", leases)
	}
}

func TestPreviewExecuteRequiresDispatch(t *testing.T) {
	err := previewExecuteCall(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "--dispatch") {
		t.Fatalf("err=%v", err)
	}
}

func TestPreviewExecuteUsesGitHubOIDCAndExactCIRoute(t *testing.T) {
	const (
		requestToken = "request-token-secret"
		oidcToken    = "oidc-jwt-secret"
		session      = "epci_session-secret"
		dispatchID   = "22222222-2222-2222-2222-222222222222"
		operationID  = "11111111-1111-1111-1111-111111111111"
	)
	var mu sync.Mutex
	var paths []string
	var sawAudience bool
	reported := false
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
		switch {
		case r.URL.Path == "/token":
			if r.URL.Query().Get("audience") != "envtie" || r.Header.Get("Authorization") != "bearer "+requestToken {
				t.Errorf("oidc request audience %q or authorization was wrong", r.URL.Query().Get("audience"))
			}
			mu.Lock()
			sawAudience = true
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"value": oidcToken})
		case r.URL.Path == "/v1/github/actions/exchange":
			if r.Header.Get("Authorization") != "Bearer "+oidcToken {
				t.Error("exchange did not present the GitHub OIDC token")
			}
			var body struct {
				DispatchID string `json:"dispatch_id"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.DispatchID != dispatchID {
				t.Error("exchange body")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"session": session, "operation_id": operationID,
				"public_key": base64.StdEncoding.EncodeToString(pub),
			})
		case r.URL.Path == "/v1/ci/jobs/"+operationID+"/lease":
			if r.Header.Get("Authorization") != "Bearer "+session {
				t.Error("lease did not use the CI session")
			}
			_ = json.NewEncoder(w).Encode(job)
		case strings.HasPrefix(r.URL.Path, "/v1/ci/jobs/"+operationID+"/"):
			if r.Header.Get("Authorization") != "Bearer "+session {
				t.Error("ci callback did not use the CI session")
			}
			if strings.HasSuffix(r.URL.Path, "/report") {
				mu.Lock()
				reported = true
				mu.Unlock()
			}
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/v1/ci/operations/"+operationID:
			if r.Header.Get("Authorization") != "Bearer "+session {
				t.Error("status did not use the CI session")
			}
			_ = json.NewEncoder(w).Encode(previewOperation{Status: "completed"})
		default:
			t.Errorf("unexpected path %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("ENVTIE_API_URL", srv.URL)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"/token")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", requestToken)
	stdout, stderr := os.Stdout, os.Stderr
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdout, os.Stderr = outW, errW
	call := func(_ context.Context, _ core.Component, req adapter.Request) (adapter.Response, error) {
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
	}
	err = previewExecuteCall([]string{"--dispatch", dispatchID, "--timeout", "30s"}, call)
	outW.Close()
	errW.Close()
	os.Stdout, os.Stderr = stdout, stderr
	out, _ := io.ReadAll(outR)
	errOut, _ := io.ReadAll(errR)
	combined := string(out) + string(errOut)
	if err != nil {
		t.Fatal(err)
	}
	if !sawAudience {
		t.Fatal("GitHub OIDC audience was not requested")
	}
	for _, secret := range []string{requestToken, oidcToken, session} {
		if strings.Contains(combined, secret) {
			t.Fatalf("command output included a credential")
		}
	}
	if !strings.Contains(string(out), operationID) || !strings.Contains(string(out), "completed") {
		t.Fatalf("stdout %q", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reported {
		t.Fatal("operation was not reported")
	}
	joined := strings.Join(paths, "\n")
	for _, needle := range []string{
		"GET /token",
		"POST /v1/github/actions/exchange",
		"POST /v1/ci/jobs/" + operationID + "/lease",
		"POST /v1/ci/jobs/" + operationID + "/checkpoint",
		"POST /v1/ci/jobs/" + operationID + "/report",
		"GET /v1/ci/operations/" + operationID,
	} {
		if !strings.Contains(joined, needle) {
			t.Fatalf("missing %s in\n%s", needle, joined)
		}
	}
	if strings.Contains(joined, "/v1/runner/jobs") {
		t.Fatal("preview execute used the workspace runner queue")
	}
}

func TestPreviewReportUsesFreshGitHubOIDC(t *testing.T) {
	const (
		requestToken = "request-token-secret"
		oidcToken    = "report-oidc-secret"
		dispatchID   = "22222222-2222-2222-2222-222222222222"
	)
	sawReport := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.URL.Query().Get("audience") != "envtie" || r.Header.Get("Authorization") != "bearer "+requestToken {
				t.Error("invalid OIDC request")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"value": oidcToken})
		case "/v1/github/actions/report":
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+oidcToken {
				t.Error("report did not present the GitHub OIDC token")
			}
			var in struct {
				DispatchID string `json:"dispatch_id"`
				Status     string `json:"status"`
			}
			if json.NewDecoder(r.Body).Decode(&in) != nil || in.DispatchID != dispatchID || in.Status != "failure" {
				t.Errorf("report body: %+v", in)
			}
			sawReport = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("ENVTIE_API_URL", srv.URL)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"/token")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", requestToken)
	if err := previewReport([]string{"--dispatch", dispatchID, "--status", "failure"}); err != nil {
		t.Fatal(err)
	}
	if !sawReport {
		t.Fatal("preview result was not reported")
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
	t.Setenv("GITHUB_REPOSITORY", "acme/app")
	githubAPI = srv.URL
	t.Cleanup(func() { githubAPI = "https://api.github.com" })
	cfg := runnerConfig{}
	if err := cfg.deliverGitHubEnvironment(context.Background(), "pr-7", "github.environment.secret.DATABASE_URL", secret); err != nil {
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
	err := (runnerConfig{}).deliverGitHubEnvironment(context.Background(), "pr-7", "github.environment.secret.DATABASE_URL", "secret")
	if err == nil || created {
		t.Fatalf("missing environment was created: err=%v created=%v", err, created)
	}
}

func TestGitHubForkDeliveryFails(t *testing.T) {
	t.Setenv("ENVTIE_FORK", "true")
	t.Setenv("GITHUB_TOKEN", "installation-token")
	t.Setenv("GITHUB_REPOSITORY", "acme/app")
	err := (runnerConfig{}).deliverGitHubEnvironment(context.Background(), "pr-7", "github.environment.secret.DATABASE_URL", "secret")
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
