package e2e_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coddicat/envtie-core/core"
	plansig "github.com/coddicat/envtie-core/plan"
)

type runnerJob struct {
	OperationID string           `json:"operation_id"`
	WorkspaceID string           `json:"workspace_id"`
	Kind        string           `json:"kind"`
	Plan        core.Plan        `json:"plan"`
	Environment core.Environment `json:"environment"`
}

type runnerReport struct {
	Environment core.Environment `json:"environment"`
	Success     bool             `json:"success"`
}

func TestPackedNPMRunnerConsumerLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("npm consumer lifecycle is an integration test")
	}
	npm := commandPath(t, "npm")
	npx := commandPath(t, "npx")
	repo := repoRoot(t)
	temp := t.TempDir()
	assets := filepath.Join(temp, "assets")
	generated := filepath.Join(temp, "generated")
	consumer := filepath.Join(temp, "test-consumer-envtie")
	stateDir := filepath.Join(temp, "provider")
	cacheDir := filepath.Join(temp, "cache")
	for _, dir := range []string{assets, consumer, stateDir, cacheDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	coreBinary := filepath.Join(temp, executable("envtie"))
	adapterBinary := filepath.Join(temp, executable("adapter-neon"))
	goTool := filepath.Join(runtime.GOROOT(), "bin", executable("go"))
	run(t, repo, nil, goTool, "build", "-trimpath", "-o", coreBinary, "./cmd/envtie")
	run(t, repo, nil, goTool, "build", "-trimpath", "-o", adapterBinary, "./e2e/fixtures/neon-adapter")

	const tag = "v0.0.1-test"
	for _, target := range []struct {
		os, arch string
	}{
		{"darwin", "amd64"}, {"darwin", "arm64"}, {"linux", "amd64"},
		{"linux", "arm64"}, {"windows", "amd64"}, {"windows", "arm64"},
	} {
		ext := ""
		if target.os == "windows" {
			ext = ".exe"
		}
		copyFile(t, coreBinary, filepath.Join(assets, fmt.Sprintf("envtie_%s_%s_%s%s", tag, target.os, target.arch, ext)))
	}
	run(t, repo, nil, commandPath(t, "node"), "scripts/package-npm.mjs", tag, assets, generated)

	nodeOS, nodeArch := npmTarget()
	platformDir := filepath.Join(generated, "cli-"+nodeOS+"-"+nodeArch)
	platformTarball := pack(t, npm, platformDir, consumer)
	launcherTarball := pack(t, npm, filepath.Join(generated, "cli"), consumer)
	packageJSON := fmt.Sprintf(`{"private":true,"dependencies":{"@envtie.dev/cli":"file:%s","@envtie.dev/cli-%s-%s":"file:%s"}}`,
		filepath.Base(launcherTarball), nodeOS, nodeArch, filepath.Base(platformTarball))
	if err := os.WriteFile(filepath.Join(consumer, "package.json"), []byte(packageJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, consumer, nil, npm, "install", "--ignore-scripts", "--no-audit", "--no-fund")

	adapterBytes, err := os.ReadFile(adapterBinary)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(adapterBytes)
	digest := hex.EncodeToString(sum[:])
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	var artifactRequests atomic.Int32
	var mu sync.Mutex
	var queued *runnerJob
	var leased bool
	var report runnerReport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/adapter-neon":
			artifactRequests.Add(1)
			_, _ = w.Write(adapterBytes)
		case r.URL.Path == "/v1/runner/jobs" && r.Method == http.MethodPost:
			mu.Lock()
			defer mu.Unlock()
			if queued == nil || leased {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			leased = true
			_ = json.NewEncoder(w).Encode(queued)
		case strings.HasSuffix(r.URL.Path, "/checkpoint"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/report"):
			var next runnerReport
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&next); err != nil {
				http.Error(w, "bad report", http.StatusBadRequest)
				return
			}
			mu.Lock()
			report = next
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	component := core.Component{
		ID: "postgres", Adapter: "neon@1", Ownership: core.Managed,
		Artifacts: map[string]core.AdapterArtifact{
			runtime.GOOS + "/" + runtime.GOARCH: {
				Release: tag, URL: server.URL + "/adapter-neon", SHA256: digest, Size: int64(len(adapterBytes)),
			},
		},
	}
	current := core.Environment{Name: "consumer-test", State: "PLANNED", Resources: map[string]core.Resource{}}
	runJob := func(kind string) core.Environment {
		now := time.Now().UTC()
		plan := core.Plan{
			ID: kind + "-" + fmt.Sprint(now.UnixNano()), Environment: current.Name,
			CreatedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour),
			Actions:  []core.Action{{Component: component, Kind: kind, Reason: "consumer lifecycle"}},
			Bindings: []core.Binding{{From: "postgres.DATABASE_URL", To: "dotenv.DATABASE_URL"}},
		}
		if err := plansig.Sign(&plan, "workspace-test", privateKey); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		queued = &runnerJob{
			OperationID: plan.ID, WorkspaceID: "workspace-test", Kind: kind, Plan: plan, Environment: current,
		}
		leased = false
		report = runnerReport{}
		mu.Unlock()
		env := append(os.Environ(),
			"ENVTIE_RUNNER_TOKEN=consumer-runner-token",
			"ENVTIE_PUBLIC_KEY="+base64.StdEncoding.EncodeToString(publicKey),
			"ENVTIE_ADAPTER_CACHE="+cacheDir,
			"ENVTIE_E2E_STATE_DIR="+stateDir,
			"NEON_API_KEY=fixture-only",
		)
		run(t, consumer, env, npx, "--no-install", "envtie", "runner", "start", "--api-url", server.URL, "--once")
		mu.Lock()
		defer mu.Unlock()
		if !report.Success {
			t.Fatalf("%s was not reported successful: %+v", kind, report.Environment)
		}
		return report.Environment
	}

	current = runJob("apply")
	if current.State != "READY" {
		t.Fatalf("apply state = %s", current.State)
	}
	binding := filepath.Join(consumer, ".envtie", "bindings", current.Name+".env")
	if body, err := os.ReadFile(binding); err != nil || !strings.Contains(string(body), "postgres://fixture.invalid/db") {
		t.Fatalf("consumer binding missing: %q, %v", body, err)
	}
	current = runJob("apply") // A fresh process must reuse the verified adapter cache.
	current = runJob("destroy")
	if current.State != "DESTROYED" {
		t.Fatalf("destroy state = %s", current.State)
	}
	if _, err := os.Stat(binding); !os.IsNotExist(err) {
		t.Fatalf("destroy did not remove binding: %v", err)
	}
	if got := artifactRequests.Load(); got != 1 {
		t.Fatalf("selected adapter downloaded %d times, want 1", got)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(working)
}

func executable(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func commandPath(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" && (name == "npm" || name == "npx") {
		name += ".cmd"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is required for the packed npm lifecycle", name)
	}
	return path
}

func run(t *testing.T, dir string, env []string, command string, args ...string) string {
	t.Helper()
	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", command, args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	body, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, body, 0o755); err != nil {
		t.Fatal(err)
	}
}

func pack(t *testing.T, npm, source, destination string) string {
	t.Helper()
	name := run(t, source, nil, npm, "pack", "--silent", "--pack-destination", destination)
	lines := strings.Fields(name)
	if len(lines) == 0 {
		t.Fatal("npm pack returned no tarball")
	}
	return filepath.Join(destination, lines[len(lines)-1])
}

func npmTarget() (string, string) {
	nodeOS := runtime.GOOS
	if nodeOS == "windows" {
		nodeOS = "win32"
	}
	nodeArch := runtime.GOARCH
	if nodeArch == "amd64" {
		nodeArch = "x64"
	}
	return nodeOS, nodeArch
}
