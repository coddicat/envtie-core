package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coddicat/envtie-core/core"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

func TestApplyRecoversLostCreateResponseWithoutPersistingSecret(t *testing.T) {
	component := core.Component{ID: "db", Adapter: "fake@1", Executable: "fake", SHA256: strings.Repeat("a", 64), Ownership: core.Managed}
	b := core.Blueprint{Name: "preview", Components: []core.Component{component}, Bindings: []core.Binding{{From: "db.DATABASE_URL", To: "env.ENVTIE_TEST_DATABASE_URL"}}}
	p, err := core.NewPlan(b, "pr-481", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	env := core.Environment{Name: "pr-481", Resources: map[string]core.Resource{}}
	created := false
	applyCalls := 0
	saves := 0
	e := Engine{Save: func(*core.Environment) error { saves++; return nil }, Call: func(_ context.Context, _ core.Component, r adapter.Request) (adapter.Response, error) {
		switch r.Hook {
		case "manifest":
			return adapter.Response{Manifest: &adapter.Manifest{Name: "fake", Version: "1.0.0", Protocol: 1, Capabilities: []string{"read", "apply", "validate", "outputs", "destroy"}, Outputs: []adapter.Field{{Name: "DATABASE_URL", Sensitivity: "SECRET"}}}}, nil
		case "read":
			if created {
				return adapter.Response{ExternalID: "safe-id", Status: "ready"}, nil
			}
			return adapter.Response{Absent: true}, nil
		case "apply":
			applyCalls++
			created = true
			return adapter.Response{}, errors.New("lost response")
		case "validate":
			return adapter.Response{Status: "ready"}, nil
		case "outputs":
			return adapter.Response{Outputs: map[string]string{"DATABASE_URL": "postgresql://secret-value"}}, nil
		}
		return adapter.Response{}, errors.New("unexpected hook")
	}}
	defer os.Unsetenv("ENVTIE_TEST_DATABASE_URL")
	if err := e.Apply(context.Background(), p, &env); err != nil {
		t.Fatal(err)
	}
	if env.State != "READY" || applyCalls != 1 || saves < 3 {
		t.Fatalf("state=%s calls=%d saves=%d", env.State, applyCalls, saves)
	}
	if os.Getenv("ENVTIE_TEST_DATABASE_URL") != "postgresql://secret-value" {
		t.Fatal("binding absent")
	}
	serialized, _ := json.Marshal(env)
	if strings.Contains(string(serialized), "secret-value") || strings.Contains(string(serialized), "postgresql://") {
		t.Fatal("secret persisted in environment state")
	}
	messages := make([]string, 0, len(env.Logs))
	for i, line := range env.Logs {
		if line.Seq != i+1 || line.Level != "info" {
			t.Fatalf("log %+v", line)
		}
		messages = append(messages, line.Message)
	}
	for _, want := range []string{"apply started", "db read", "db apply", "db ready", "bindings written", "apply completed"} {
		if !strings.Contains(strings.Join(messages, "\n"), want) {
			t.Fatalf("missing %q in %v", want, messages)
		}
	}
}

func TestAppendLogDropsUnsafeText(t *testing.T) {
	env := core.Environment{}
	appendLog(&env, "info", "postgres apply")
	appendLog(&env, "error", "https://console.example/secret")
	appendLog(&env, "info", "Token=abc")
	if len(env.Logs) != 1 || env.Logs[0].Message != "postgres apply" {
		t.Fatalf("%+v", env.Logs)
	}
}

func creationOnlyEngine(t *testing.T, readID string, applyCalls *int) Engine {
	t.Helper()
	return Engine{Call: func(_ context.Context, _ core.Component, r adapter.Request) (adapter.Response, error) {
		switch r.Hook {
		case "manifest":
			return adapter.Response{Manifest: &adapter.Manifest{Name: "fake", Version: "1.0.0", Protocol: 1, Capabilities: []string{"read", "apply", "validate", "outputs", "destroy"}, Outputs: []adapter.Field{
				{Name: "SIGNING_SECRET", Sensitivity: "SECRET", Availability: adapter.AvailabilityCreate},
				{Name: "ENDPOINT_ID", Sensitivity: "PUBLIC"},
			}}}, nil
		case "read":
			if readID != "" {
				return adapter.Response{ExternalID: readID, Status: "ready"}, nil
			}
			return adapter.Response{Absent: true}, nil
		case "apply":
			*applyCalls++
			return adapter.Response{ExternalID: "we_1", Status: "ready", Outputs: map[string]string{"SIGNING_SECRET": "whsec_created-once", "ENDPOINT_ID": "ignored"}}, nil
		case "validate":
			return adapter.Response{Status: "ready"}, nil
		case "outputs":
			return adapter.Response{Outputs: map[string]string{"ENDPOINT_ID": "we_1"}}, nil
		}
		return adapter.Response{}, errors.New("unexpected hook")
	}}
}

func creationOnlyPlan(t *testing.T) core.Plan {
	t.Helper()
	component := core.Component{ID: "hook", Adapter: "fake@1", Executable: "fake", SHA256: strings.Repeat("a", 64), Ownership: core.Managed}
	b := core.Blueprint{Name: "preview", Components: []core.Component{component}, Bindings: []core.Binding{
		{From: "hook.SIGNING_SECRET", To: "env.ENVTIE_TEST_SIGNING_SECRET"},
		{From: "hook.ENDPOINT_ID", To: "env.ENVTIE_TEST_ENDPOINT_ID"},
	}}
	p, err := core.NewPlan(b, "pr-7", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCreationOnlyOutputBoundOnCreateAndNeverPersisted(t *testing.T) {
	defer os.Unsetenv("ENVTIE_TEST_SIGNING_SECRET")
	defer os.Unsetenv("ENVTIE_TEST_ENDPOINT_ID")
	calls := 0
	env := core.Environment{Name: "pr-7", State: "PLANNED", Resources: map[string]core.Resource{}}
	if err := creationOnlyEngine(t, "", &calls).Apply(context.Background(), creationOnlyPlan(t), &env); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || env.State != "READY" {
		t.Fatalf("calls=%d state=%s", calls, env.State)
	}
	if os.Getenv("ENVTIE_TEST_SIGNING_SECRET") != "whsec_created-once" {
		t.Fatal("creation-only output not bound")
	}
	if os.Getenv("ENVTIE_TEST_ENDPOINT_ID") != "we_1" {
		t.Fatal("regular output must come from the outputs hook")
	}
	serialized, _ := json.Marshal(env)
	if strings.Contains(string(serialized), "whsec_") {
		t.Fatal("creation-only secret persisted in environment state")
	}
}

func TestCreationOnlyOutputSkippedOnUpdateOfReadyEnvironment(t *testing.T) {
	os.Unsetenv("ENVTIE_TEST_SIGNING_SECRET")
	defer os.Unsetenv("ENVTIE_TEST_ENDPOINT_ID")
	calls := 0
	env := core.Environment{Name: "pr-7", State: "READY", Resources: map[string]core.Resource{"hook": {ComponentID: "hook", Adapter: "fake@1", Ownership: core.Managed, ExternalID: "we_1", Status: "ready"}}}
	if err := creationOnlyEngine(t, "we_1", &calls).Apply(context.Background(), creationOnlyPlan(t), &env); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || env.State != "READY" {
		t.Fatalf("calls=%d state=%s", calls, env.State)
	}
	if _, set := os.LookupEnv("ENVTIE_TEST_SIGNING_SECRET"); set {
		t.Fatal("creation-only output must not be re-bound")
	}
	if os.Getenv("ENVTIE_TEST_ENDPOINT_ID") != "we_1" {
		t.Fatal("regular output not bound on update")
	}
}

func TestCreationOnlyOutputLostFailsLoudly(t *testing.T) {
	calls := 0
	env := core.Environment{Name: "pr-7", State: "RUNNING", Resources: map[string]core.Resource{"hook": {ComponentID: "hook", Adapter: "fake@1", Ownership: core.Managed, ExternalID: "we_1", Status: "ready"}}}
	err := creationOnlyEngine(t, "we_1", &calls).Apply(context.Background(), creationOnlyPlan(t), &env)
	if err == nil || !strings.Contains(err.Error(), "destroy and recreate") {
		t.Fatalf("expected loud failure, got %v", err)
	}
	if env.State != "DEGRADED" {
		t.Fatalf("state=%s", env.State)
	}
}

func TestReferencedNeverDestroyed(t *testing.T) {
	b := core.Blueprint{Name: "test", Components: []core.Component{{ID: "shared", Adapter: "fake@1", Executable: "fake", SHA256: strings.Repeat("a", 64), Ownership: core.Referenced, Config: map[string]string{"id": "safe-id"}}}}
	env := core.Environment{Name: "preview", Resources: map[string]core.Resource{"shared": {ComponentID: "shared", Adapter: "fake@1", Ownership: core.Referenced, ExternalID: "safe-id"}}}
	calls := 0
	e := Engine{Call: func(context.Context, core.Component, adapter.Request) (adapter.Response, error) {
		calls++
		return adapter.Response{}, nil
	}}
	if err := e.Destroy(context.Background(), b, &env, ""); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("referenced adapter called during destroy")
	}
}

func TestProtectedDestroyRequiresExactName(t *testing.T) {
	env := core.Environment{Name: "staging", Protected: true, Resources: map[string]core.Resource{}}
	b := core.Blueprint{Name: "test", Components: []core.Component{{ID: "db", Adapter: "fake@1", Executable: "fake", SHA256: strings.Repeat("a", 64), Ownership: core.Managed}}}
	if err := (Engine{}).Destroy(context.Background(), b, &env, "stage"); err == nil {
		t.Fatal("protected environment accepted wrong confirmation")
	}
	if env.State == "DESTROYED" {
		t.Fatal("protected environment mutated")
	}
}
func TestDestroyResidual(t *testing.T) {
	b := core.Blueprint{Name: "test", Components: []core.Component{{ID: "db", Adapter: "fake@1", Executable: "fake", SHA256: strings.Repeat("a", 64), Ownership: core.Managed}}}
	env := core.Environment{Name: "preview", Resources: map[string]core.Resource{"db": {ComponentID: "db", Adapter: "fake@1", Ownership: core.Managed, ExternalID: "res-1"}}}
	e := Engine{Call: func(_ context.Context, _ core.Component, r adapter.Request) (adapter.Response, error) {
		if r.Hook == "manifest" {
			return adapter.Response{Manifest: &adapter.Manifest{Name: "fake", Version: "1.0.0", Protocol: 1, Capabilities: []string{"read", "validate", "outputs", "apply", "destroy"}}}, nil
		}
		if r.Hook == "read" {
			return adapter.Response{Absent: false}, nil
		}
		return adapter.Response{}, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e.Destroy(ctx, b, &env, "") == nil {
		t.Fatal("unverified destroy reported success")
	}
	if env.State != "RESIDUAL" || len(env.Residuals) != 1 || env.FailureCode != "residual_resources" || env.FailurePhase != "destroy" || env.LastError != "some managed resources remain" {
		t.Fatalf("state=%s residuals=%v code=%s last=%q", env.State, env.Residuals, env.FailureCode, env.LastError)
	}
}

func TestDestroyAccountsForResourcesOmittedFromBlueprint(t *testing.T) {
	b := core.Blueprint{Name: "test", Components: []core.Component{{ID: "shared", Ownership: core.Referenced}}}
	env := core.Environment{Name: "preview", Resources: map[string]core.Resource{
		"db":           {ComponentID: "db", Adapter: "fake@1", Ownership: core.Managed, ExternalID: "res-1"},
		"other_shared": {ComponentID: "other_shared", Ownership: core.Referenced, ExternalID: "shared-1"},
	}}
	calls := 0
	e := Engine{Call: func(context.Context, core.Component, adapter.Request) (adapter.Response, error) {
		calls++
		return adapter.Response{}, nil
	}}
	if err := e.Destroy(context.Background(), b, &env, ""); err == nil {
		t.Fatal("omitted managed resource reported destroyed")
	}
	if env.State != "RESIDUAL" || len(env.Residuals) != 1 || env.Residuals[0] != "db" || len(env.Resources) != 1 || calls != 0 {
		t.Fatal("destroy must retain omitted managed inventory without mutating referenced resources")
	}
}

func fakeManifest() adapter.Response {
	return adapter.Response{Manifest: &adapter.Manifest{Name: "fake", Version: "1.0.0", Protocol: 1, Capabilities: []string{"read", "apply", "validate", "outputs", "destroy"}}}
}

func managedComponent(id string) core.Component {
	return core.Component{ID: id, Adapter: "fake@1", Executable: "fake", SHA256: strings.Repeat("a", 64), Ownership: core.Managed}
}

func dotenvPath(environment string) string {
	return filepath.Join(".envtie", "bindings", environment+".env")
}

func TestDotenvBindingRetryOverwritesSortedKeys(t *testing.T) {
	t.Chdir(t.TempDir())
	first := []core.Binding{{From: "db.Z", To: "dotenv.ZEBRA"}, {From: "db.A", To: "dotenv.ALPHA"}}
	outputs := map[string]string{"db.Z": "z1", "db.A": "a1"}
	if err := bind("pr-481", first, outputs); err != nil {
		t.Fatal(err)
	}
	if err := bind("pr-481", []core.Binding{{From: "db.A", To: "dotenv.ALPHA"}, {From: "db.Z", To: "dotenv.ZEBRA"}}, outputs); err != nil {
		t.Fatal(err)
	}
	path := dotenvPath("pr-481")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ALPHA=\"a1\"\nZEBRA=\"z1\"\n" {
		t.Fatalf("retry appended or reordered: %q", got)
	}
	if _, err := os.Stat(filepath.Join(".envtie", "generated.env")); !os.IsNotExist(err) {
		t.Fatal("shared generated.env was written")
	}
	second := []core.Binding{{From: "db.B", To: "dotenv.BETA"}, {From: "db.A", To: "dotenv.ALPHA"}}
	if err := bind("pr-481", second, map[string]string{"db.B": "b2", "db.A": "a2"}); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ALPHA=\"a2\"\nBETA=\"b2\"\n" {
		t.Fatalf("overwrite: %q", got)
	}
	entries, err := os.ReadDir(filepath.Join(".envtie", "bindings"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "pr-481.env" {
		t.Fatalf("bindings dir: %#v", names(entries))
	}
}

func TestDotenvBindingsAreIsolatedByEnvironment(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := bind("pr-1", []core.Binding{{From: "db.URL", To: "dotenv.URL"}}, map[string]string{"db.URL": "one"}); err != nil {
		t.Fatal(err)
	}
	if err := bind("pr-2", []core.Binding{{From: "db.URL", To: "dotenv.TOKEN"}}, map[string]string{"db.URL": "two"}); err != nil {
		t.Fatal(err)
	}
	if err := bind("pr-1", []core.Binding{{From: "db.URL", To: "dotenv.URL"}}, map[string]string{"db.URL": "one-b"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dotenvPath("pr-1"))
	if err != nil || string(got) != "URL=\"one-b\"\n" {
		t.Fatalf("pr-1 binding: %q %v", got, err)
	}
	got, err = os.ReadFile(dotenvPath("pr-2"))
	if err != nil || string(got) != "TOKEN=\"two\"\n" {
		t.Fatalf("pr-2 binding: %q %v", got, err)
	}
	if err := bind("../pr-2", []core.Binding{{From: "db.URL", To: "dotenv.URL"}}, map[string]string{"db.URL": "escaped"}); err == nil {
		t.Fatal("unsafe environment name accepted")
	}
	if _, err := os.Stat(filepath.Join("..", "pr-2.env")); !os.IsNotExist(err) {
		t.Fatal("binding escaped the environment directory")
	}
}

func TestBindingsValidateBeforeWritingAndRejectSymlinks(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("SAFE_BINDING_TEST", "original")
	bindings := []core.Binding{
		{From: "db.URL", To: "env.SAFE_BINDING_TEST"},
		{From: "db.URL", To: "file.safe.txt"},
		{From: "db.URL", To: "file.../escape"},
	}
	if err := bind("preview", bindings, map[string]string{"db.URL": "secret"}); err == nil {
		t.Fatal("invalid binding accepted")
	}
	if got := os.Getenv("SAFE_BINDING_TEST"); got != "original" {
		t.Fatalf("environment changed before validation completed: %q", got)
	}
	if _, err := os.Stat("safe.txt"); !os.IsNotExist(err) {
		t.Fatal("file written before validation completed")
	}

	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, "linked"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := bind("preview", []core.Binding{{From: "db.URL", To: "file.linked"}}, map[string]string{"db.URL": "secret"}); err == nil {
		t.Fatal("symlink binding accepted")
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "keep" {
		t.Fatalf("symlink target changed: %q %v", got, err)
	}
}

func TestFileBindingAtomicallyReplacesWithRestrictedMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode assertion")
	}
	t.Chdir(t.TempDir())
	if err := os.WriteFile("binding.txt", []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := bind("preview", []core.Binding{{From: "db.URL", To: "file.binding.txt"}}, map[string]string{"db.URL": "new"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("binding.txt")
	if err != nil || string(got) != "new" {
		t.Fatalf("binding: %q %v", got, err)
	}
	info, err := os.Stat("binding.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v", info.Mode().Perm())
	}
}

func TestDestroyDeletesOnlyThatEnvironmentBinding(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := bind("pr-1", []core.Binding{{From: "db.URL", To: "dotenv.URL"}}, map[string]string{"db.URL": "one"}); err != nil {
		t.Fatal(err)
	}
	if err := bind("pr-2", []core.Binding{{From: "db.URL", To: "dotenv.URL"}}, map[string]string{"db.URL": "two"}); err != nil {
		t.Fatal(err)
	}
	b := core.Blueprint{Name: "test", Components: []core.Component{managedComponent("db")}}
	env := core.Environment{Name: "pr-1", Resources: map[string]core.Resource{"db": {ComponentID: "db", Adapter: "fake@1", Ownership: core.Managed, ExternalID: "res-1"}}}
	e := Engine{Call: func(_ context.Context, _ core.Component, r adapter.Request) (adapter.Response, error) {
		if r.Hook == "manifest" {
			return fakeManifest(), nil
		}
		if r.Hook == "destroy" && r.ExternalID != "res-1" {
			t.Fatalf("destroy id %s", r.ExternalID)
		}
		if r.Hook == "read" {
			return adapter.Response{Absent: true}, nil
		}
		return adapter.Response{}, nil
	}}
	if err := e.Destroy(context.Background(), b, &env, ""); err != nil || env.State != "DESTROYED" {
		t.Fatalf("destroy: %v state=%s", err, env.State)
	}
	if _, err := os.Stat(dotenvPath("pr-1")); !os.IsNotExist(err) {
		t.Fatal("destroyed environment binding remains")
	}
	got, err := os.ReadFile(dotenvPath("pr-2"))
	if err != nil || string(got) != "URL=\"two\"\n" {
		t.Fatalf("other binding: %q %v", got, err)
	}
}

func TestDestroyRetainsBindingsForResidual(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := bind("preview", []core.Binding{{From: "db.URL", To: "dotenv.URL"}}, map[string]string{"db.URL": "keep"}); err != nil {
		t.Fatal(err)
	}
	b := core.Blueprint{Name: "test", Components: []core.Component{managedComponent("db")}}
	env := core.Environment{Name: "preview", Resources: map[string]core.Resource{"db": {ComponentID: "db", Adapter: "fake@1", Ownership: core.Managed, ExternalID: "res-1"}}}
	e := Engine{Call: func(_ context.Context, _ core.Component, r adapter.Request) (adapter.Response, error) {
		if r.Hook == "manifest" {
			return fakeManifest(), nil
		}
		if r.Hook == "read" {
			return adapter.Response{Absent: false}, nil
		}
		return adapter.Response{}, nil
	}}
	if err := e.Destroy(context.Background(), b, &env, ""); err == nil || env.State != "RESIDUAL" {
		t.Fatalf("destroy: %v state=%s", err, env.State)
	}
	got, err := os.ReadFile(dotenvPath("preview"))
	if err != nil || string(got) != "URL=\"keep\"\n" {
		t.Fatalf("residual binding: %q %v", got, err)
	}
}

func TestDestroyEmptyExternalIDAbsentRemovesPlaceholder(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := bind("preview", []core.Binding{{From: "db.URL", To: "dotenv.URL"}}, map[string]string{"db.URL": "gone"}); err != nil {
		t.Fatal(err)
	}
	component := managedComponent("db")
	component.Config = map[string]string{"name": "shared-production"}
	b := core.Blueprint{Name: "test", Components: []core.Component{component}}
	env := core.Environment{Name: "preview", Resources: map[string]core.Resource{"db": {ComponentID: "db", Adapter: "fake@1", Ownership: core.Managed, Status: "creating"}}}
	e := Engine{Call: func(_ context.Context, _ core.Component, r adapter.Request) (adapter.Response, error) {
		switch r.Hook {
		case "manifest":
			return fakeManifest(), nil
		case "read":
			if r.ExternalID != "" || r.Config["name"] != "preview-db" {
				t.Fatalf("read id=%q name=%q", r.ExternalID, r.Config["name"])
			}
			return adapter.Response{Absent: true}, nil
		case "destroy":
			t.Fatalf("destroy called for absent placeholder: %s", r.ExternalID)
		}
		return adapter.Response{}, errors.New("unexpected hook")
	}}
	if err := e.Destroy(context.Background(), b, &env, ""); err != nil || env.State != "DESTROYED" || len(env.Resources) != 0 {
		t.Fatalf("destroy: %v state=%s resources=%v", err, env.State, env.Resources)
	}
	if _, err := os.Stat(dotenvPath("preview")); !os.IsNotExist(err) {
		t.Fatal("binding retained after verified destroy")
	}
}

func TestDestroyRecoversEmptyExternalID(t *testing.T) {
	t.Chdir(t.TempDir())
	component := managedComponent("db")
	component.Config = map[string]string{"name": "shared-production", "project_id": "proj"}
	b := core.Blueprint{Name: "test", Components: []core.Component{component}}
	env := core.Environment{Name: "preview", Resources: map[string]core.Resource{"db": {ComponentID: "db", Adapter: "fake@1", Ownership: core.Managed, Status: "creating"}}}
	var destroyed []string
	e := Engine{Call: func(_ context.Context, _ core.Component, r adapter.Request) (adapter.Response, error) {
		switch r.Hook {
		case "manifest":
			return fakeManifest(), nil
		case "read":
			if r.ExternalID == "" {
				if r.Config["name"] != "preview-db" {
					return adapter.Response{ExternalID: "unrelated-id"}, nil
				}
				if r.Config["project_id"] != "proj" {
					t.Fatalf("lookup config: %#v", r.Config)
				}
				return adapter.Response{ExternalID: "recovered-id"}, nil
			}
			if r.ExternalID != "recovered-id" {
				t.Fatalf("verify read id %s", r.ExternalID)
			}
			return adapter.Response{Absent: true}, nil
		case "destroy":
			destroyed = append(destroyed, r.ExternalID)
			return adapter.Response{}, nil
		default:
			return adapter.Response{}, errors.New("unexpected hook")
		}
	}}
	if err := e.Destroy(context.Background(), b, &env, ""); err != nil || env.State != "DESTROYED" || len(env.Resources) != 0 {
		t.Fatalf("destroy: %v state=%s resources=%v", err, env.State, env.Resources)
	}
	if len(destroyed) != 1 || destroyed[0] != "recovered-id" {
		t.Fatalf("destroyed %#v", destroyed)
	}
}

func TestDestroyUsesStoredIDNotNameCollision(t *testing.T) {
	component := managedComponent("db")
	component.Config = map[string]string{"name": "preview-db"}
	b := core.Blueprint{Name: "test", Components: []core.Component{component}}
	env := core.Environment{Name: "preview", Resources: map[string]core.Resource{"db": {ComponentID: "db", Adapter: "fake@1", Ownership: core.Managed, ExternalID: "ours"}}}
	var destroyed []string
	e := Engine{Call: func(_ context.Context, _ core.Component, r adapter.Request) (adapter.Response, error) {
		switch r.Hook {
		case "manifest":
			return fakeManifest(), nil
		case "read":
			if r.ExternalID == "" {
				t.Fatal("name lookup replaced a stored provider id")
				return adapter.Response{ExternalID: "unrelated-id"}, nil
			}
			if r.ExternalID != "ours" {
				t.Fatalf("read id %s", r.ExternalID)
			}
			return adapter.Response{Absent: true}, nil
		case "destroy":
			destroyed = append(destroyed, r.ExternalID)
			return adapter.Response{}, nil
		default:
			return adapter.Response{}, errors.New("unexpected hook")
		}
	}}
	if err := e.Destroy(context.Background(), b, &env, ""); err != nil || env.State != "DESTROYED" {
		t.Fatalf("destroy: %v state=%s", err, env.State)
	}
	if len(destroyed) != 1 || destroyed[0] != "ours" {
		t.Fatalf("destroyed %#v", destroyed)
	}
}

func TestDiagnosticsExposeComponentHookAndCodeOnly(t *testing.T) {
	const leak = "postgresql://role:secret@ep.neon.tech/db https://console.neon.tech BODY-MARKER"
	plan := diagnosticPlan(t, "db")
	t.Run("read", func(t *testing.T) {
		env := core.Environment{Name: "preview", State: "PLANNED", Resources: map[string]core.Resource{}}
		err := diagnosticEngine(nil, func(r adapter.Request) (adapter.Response, error) {
			if r.Hook != "read" {
				t.Fatalf("unexpected hook %s", r.Hook)
			}
			return adapter.Response{}, fmt.Errorf("%s: %w", leak, adapter.NewError("neon_quota"))
		}).Apply(context.Background(), plan, &env)
		assertDiagnostic(t, err, env, "db", "read", "neon_quota", "FAILED", leak)
	})
	t.Run("uncoded", func(t *testing.T) {
		env := core.Environment{Name: "preview", State: "PLANNED", Resources: map[string]core.Resource{}}
		err := diagnosticEngine(nil, func(r adapter.Request) (adapter.Response, error) {
			if r.Hook != "read" {
				t.Fatalf("unexpected hook %s", r.Hook)
			}
			return adapter.Response{}, errors.New(leak)
		}).Apply(context.Background(), plan, &env)
		assertDiagnostic(t, err, env, "db", "read", "adapter_failed", "FAILED", leak)
	})
	t.Run("outputs", func(t *testing.T) {
		prev := retryBackoff
		retryBackoff = 0
		t.Cleanup(func() { retryBackoff = prev })
		env := core.Environment{Name: "preview", State: "READY", Resources: map[string]core.Resource{
			"db": {ComponentID: "db", Adapter: "fake@1", Ownership: core.Managed, ExternalID: "br-1", Status: "ready"},
		}}
		err := diagnosticEngine(nil, func(r adapter.Request) (adapter.Response, error) {
			switch r.Hook {
			case "validate":
				return adapter.Response{Status: "ready"}, nil
			case "outputs":
				return adapter.Response{}, fmt.Errorf("%s: %w", leak, adapter.NewError("neon_outputs_unavailable"))
			default:
				t.Fatalf("unexpected hook %s", r.Hook)
			}
			return adapter.Response{}, nil
		}).Apply(context.Background(), plan, &env)
		assertDiagnostic(t, err, env, "db", "outputs", "neon_outputs_unavailable", "DEGRADED", leak)
	})
	t.Run("manifest code", func(t *testing.T) {
		env := core.Environment{Name: "preview", State: "PLANNED", Resources: map[string]core.Resource{}}
		err := diagnosticEngine(func(r adapter.Request) (adapter.Response, error) {
			if r.Hook != "manifest" {
				t.Fatal("manifest failure continued")
			}
			return adapter.Response{}, fmt.Errorf("%s: %w", leak, adapter.NewError("neon_unauthorized"))
		}, nil).Apply(context.Background(), plan, &env)
		if env.State != "PLANNED" || env.LastError != "" {
			t.Fatalf("state=%s last=%q", env.State, env.LastError)
		}
		var failure *adapter.Failure
		if !errors.As(err, &failure) || failure.Code != "neon_unauthorized" || failure.Component != "db" || failure.Hook != "manifest" {
			t.Fatalf("%v", err)
		}
		if strings.Contains(err.Error(), "postgresql://") || strings.Contains(err.Error(), "BODY-MARKER") || strings.Contains(err.Error(), "https://") {
			t.Fatal(err)
		}
	})
	t.Run("manifest detail dropped", func(t *testing.T) {
		env := core.Environment{Name: "preview", State: "PLANNED"}
		err := diagnosticEngine(func(adapter.Request) (adapter.Response, error) {
			return adapter.Response{}, errors.New(leak)
		}, nil).Apply(context.Background(), plan, &env)
		if err == nil || err.Error() != "db: adapter contract invalid" || strings.Contains(err.Error(), "BODY-MARKER") {
			t.Fatal(err)
		}
	})
}

func diagnosticPlan(t *testing.T, id string) core.Plan {
	t.Helper()
	b := core.Blueprint{Name: "preview", Components: []core.Component{managedComponent(id)}}
	p, err := core.NewPlan(b, "preview", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func diagnosticEngine(manifest, fn func(adapter.Request) (adapter.Response, error)) Engine {
	return Engine{Call: func(_ context.Context, _ core.Component, r adapter.Request) (adapter.Response, error) {
		if r.Hook == "manifest" {
			if manifest != nil {
				return manifest(r)
			}
			return fakeManifest(), nil
		}
		return fn(r)
	}}
}

func assertDiagnostic(t *testing.T, err error, env core.Environment, component, hook, code, state, leak string) {
	t.Helper()
	var failure *adapter.Failure
	var coded *adapter.CodedError
	if !errors.As(err, &failure) || failure.Code != code || failure.Component != component || failure.Hook != hook {
		t.Fatalf("%v", err)
	}
	if !errors.As(err, &coded) || coded.Code != code || err.Error() != component+" "+hook+": "+code {
		t.Fatalf("code: %v", err)
	}
	if env.State != state || env.LastError != err.Error() || env.FailureCode != code || env.FailureComponent != component || env.FailurePhase != hook {
		t.Fatalf("state=%s last=%q code=%s component=%s phase=%s err=%v", env.State, env.LastError, env.FailureCode, env.FailureComponent, env.FailurePhase, err)
	}
	serialized, _ := json.Marshal(env)
	for _, text := range []string{err.Error(), env.LastError, string(serialized)} {
		if strings.Contains(text, "postgresql://") || strings.Contains(text, "https://") || strings.Contains(text, "BODY-MARKER") || strings.Contains(text, leak) {
			t.Fatalf("diagnostic leaked provider detail: %s", text)
		}
	}
}

func TestNoteFailureRecordsUnsavedManifestDiagnostic(t *testing.T) {
	env := core.Environment{Name: "preview", State: "PLANNED", Resources: map[string]core.Resource{}}
	err := adapter.WithPlace("db", "manifest", adapter.NewError("neon_unauthorized"))
	NoteFailure(&env, err)
	if env.State != "FAILED" || env.FailureCode != "neon_unauthorized" || env.FailureComponent != "db" || env.FailurePhase != "manifest" || env.LastError != err.Error() {
		t.Fatalf("state=%s last=%q code=%s err=%v", env.State, env.LastError, env.FailureCode, err)
	}
	NoteFailure(&env, adapter.WithPlace("db", "read", adapter.NewError("https://user:secret@example.com/BODY-MARKER")))
	if env.FailureCode != "neon_unauthorized" || strings.Contains(env.LastError, "BODY-MARKER") || strings.Contains(env.LastError, "https://") {
		t.Fatalf("existing diagnostic replaced: %+v", env)
	}
}

func TestNoteFailureMakesUncodedFailureReportable(t *testing.T) {
	env := core.Environment{Name: "preview", State: "PLANNED", Resources: map[string]core.Resource{}}
	NoteFailure(&env, errors.New("db: adapter contract invalid"))
	if env.State != "FAILED" || env.FailureCode != "" || env.LastError == "" {
		t.Fatalf("state=%s last=%q code=%s", env.State, env.LastError, env.FailureCode)
	}
	ready := core.Environment{Name: "preview", State: "READY"}
	NoteFailure(&ready, errors.New("protected environment"))
	if ready.State != "READY" || ready.LastError != "" {
		t.Fatalf("ready environment changed: %+v", ready)
	}
}

func TestChecksumMismatchIsCodedManifestFailure(t *testing.T) {
	plan := diagnosticPlan(t, "db")
	env := core.Environment{Name: "preview", State: "PLANNED", Resources: map[string]core.Resource{}}
	err := diagnosticEngine(func(adapter.Request) (adapter.Response, error) {
		return adapter.Response{}, adapter.NewError("adapter_checksum_mismatch")
	}, nil).Apply(context.Background(), plan, &env)
	NoteFailure(&env, err)
	if env.State != "FAILED" || env.FailureCode != "adapter_checksum_mismatch" || env.FailureComponent != "db" || env.FailurePhase != "manifest" {
		t.Fatalf("state=%s code=%s component=%s phase=%s err=%v", env.State, env.FailureCode, env.FailureComponent, env.FailurePhase, err)
	}
}

func names(entries []os.DirEntry) []string {
	out := make([]string, len(entries))
	for i, entry := range entries {
		out[i] = entry.Name()
	}
	return out
}

func TestExpiredPlanNeverCallsAdapter(t *testing.T) {
	called := false
	e := Engine{Call: func(context.Context, core.Component, adapter.Request) (adapter.Response, error) {
		called = true
		return adapter.Response{}, nil
	}}
	env := core.Environment{Name: "preview", State: "PLANNED"}
	if err := e.Apply(context.Background(), core.Plan{Environment: "preview", ExpiresAt: time.Now().Add(-time.Second)}, &env); err == nil || called || env.State != "PLANNED" {
		t.Fatal("expired plan executed or changed state")
	}
}
