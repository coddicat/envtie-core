package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coddicat/envtie-core/adapters/github"
	"github.com/coddicat/envtie-core/adapters/httpclient"
	"github.com/coddicat/envtie-core/adapters/neon"
	"github.com/coddicat/envtie-core/adapters/upstash"
	"github.com/coddicat/envtie-core/core"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

type lifecycleTransport func(*http.Request) (*http.Response, error)

func (f lifecycleTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// This exercises official adapter HTTP behavior and the shared engine without
// external accounts. It does not certify real-provider permissions or uptime.
func TestOfficialMultiProviderLifecycle(t *testing.T) {
	for _, interrupt := range []string{"none", "db", "cache", "bindings"} {
		t.Run(interrupt, func(t *testing.T) {
			t.Setenv("NEON_API_KEY", "fixture-neon")
			t.Setenv("UPSTASH_EMAIL", "fixture@example.com")
			t.Setenv("UPSTASH_API_KEY", "fixture-upstash")
			t.Setenv("GITHUB_TOKEN", "fixture-github")
			t.Setenv("GITHUB_REPOSITORY", "fixture/repo")
			t.Setenv("ENVTIE_GITHUB_ENVIRONMENT", "preview")
			ids := map[string]string{"shared": "redis-shared"}
			creates := map[string]int{}
			bound := map[string]string{}
			var deleted []string
			retainCache := false
			transport := lifecycleTransport(func(r *http.Request) (*http.Response, error) {
				status, body := 200, "{}"
				path := r.URL.Path
				switch r.URL.Host {
				case "console.neon.tech":
					if strings.HasSuffix(path, "/branches") {
						if r.Method == "POST" {
							creates["db"]++
							ids["db"] = "br-fixture"
							body = `{"branch":{"id":"br-fixture"}}`
						} else if ids["db"] != "" {
							body = `{"branches":[{"id":"br-fixture","name":"preview-db"}]}`
						} else {
							body = `{"branches":[]}`
						}
					} else if strings.HasSuffix(path, "/connection_uri") {
						body = `{"uri":"postgresql://lifecycle-secret"}`
					} else if r.Method == "DELETE" {
						delete(ids, "db")
						deleted = append(deleted, "db")
						status = 204
					} else if ids["db"] == "" {
						status = 404
					}
				case "api.upstash.com":
					if strings.HasSuffix(path, "/databases") {
						if ids["cache"] == "" {
							body = `[]`
						} else {
							body = `[{"database_id":"redis-fixture","database_name":"preview-cache"}]`
						}
					} else if r.Method == "POST" {
						creates["cache"]++
						ids["cache"] = "redis-fixture"
						body = `{"database_id":"redis-fixture"}`
					} else if strings.HasSuffix(path, "/redis-shared") {
						if r.Method != "GET" {
							t.Fatal("referenced resource mutated")
						}
						body = `{"database_id":"redis-shared","endpoint":"shared.upstash.io","password":"shared-secret"}`
					} else if r.Method == "DELETE" {
						deleted = append(deleted, "cache")
						if !retainCache {
							delete(ids, "cache")
						}
					} else if ids["cache"] == "" {
						status = 404
					} else {
						body = `{"database_id":"redis-fixture","endpoint":"fixture.upstash.io","password":"redis-secret"}`
					}
				case "api.github.com":
					if strings.Contains(path, "/pulls/") {
						body = `{"number":1,"head":{"sha":"safe-sha"}}`
					} else {
						t.Fatal("unexpected GitHub request")
					}
				default:
					t.Fatal("unexpected provider host")
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})
			oldClient, oldTransport := httpclient.Client, http.DefaultTransport
			httpclient.Client = &http.Client{Transport: transport}
			http.DefaultTransport = transport
			defer func() { httpclient.Client = oldClient; http.DefaultTransport = oldTransport }()
			b := core.Blueprint{Name: "lifecycle", Components: []core.Component{
				{ID: "db", Adapter: "neon@1", Ownership: core.Managed, Config: map[string]string{"project_id": "fixture-project", "database_name": "neondb", "role_name": "owner"}},
				{ID: "cache", Adapter: "upstash@1", Ownership: core.Managed, DependsOn: []string{"db"}},
				{ID: "shared", Adapter: "upstash@1", Ownership: core.Referenced, Config: map[string]string{"id": "redis-shared"}},
				{ID: "context", Adapter: "github@1", Ownership: core.Referenced, DependsOn: []string{"cache"}, Config: map[string]string{"id": "1", "pull_number": "1"}},
			}, Bindings: []core.Binding{{From: "db.DATABASE_URL", To: "github.environment.secret.DATABASE_URL"}, {From: "cache.REDIS_URL", To: "github.environment.secret.REDIS_URL"}}}
			for i := range b.Components {
				b.Components[i].Executable = "fixture"
				b.Components[i].SHA256 = strings.Repeat("a", 64)
			}
			plan, err := core.NewPlan(b, "preview", nil, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			env := core.Environment{Name: "preview", State: "PLANNED", Resources: map[string]core.Resource{}}
			checkpoint, _ := json.Marshal(env)
			interrupted := false
			engine := Engine{Call: func(ctx context.Context, c core.Component, r adapter.Request) (adapter.Response, error) {
				var handler adapter.Handler
				switch c.Adapter {
				case "neon@1":
					handler = neon.Handler{}
				case "upstash@1":
					handler = upstash.Handler{}
				case "github@1":
					handler = github.Handler{}
				}
				if r.Hook == "manifest" {
					manifest := handler.Manifest()
					return adapter.Response{Manifest: &manifest}, nil
				}
				return handler.Handle(ctx, r)
			}, Deliver: func(_ context.Context, destination, value string) error {
				_, name, ok := core.GitHubBinding(destination)
				if !ok {
					t.Fatal("invalid GitHub destination")
				}
				bound[name] = value
				return nil
			}, Save: func(e *core.Environment) error {
				if !interrupted && (e.Resources[interrupt].Status == "created" || interrupt == "bindings" && e.State == "READY") {
					interrupted = true
					return errors.New("simulated interruption")
				}
				checkpoint, _ = json.Marshal(e)
				for _, secret := range []string{"lifecycle-secret", "redis-secret", "shared-secret", "fixture-neon", "fixture-github"} {
					if strings.Contains(string(checkpoint), secret) {
						t.Fatal("secret entered checkpoint")
					}
				}
				return nil
			}}
			err = engine.Apply(context.Background(), plan, &env)
			if interrupt != "none" {
				if err == nil || !interrupted {
					t.Fatal("interruption did not stop execution")
				}
				if json.Unmarshal(checkpoint, &env) != nil {
					t.Fatal("invalid sanitized checkpoint")
				}
				err = engine.Apply(context.Background(), plan, &env)
			}
			if err != nil || env.State != "READY" || creates["db"] != 1 || creates["cache"] != 1 {
				t.Fatal("resume did not reach READY without duplicates")
			}
			if bound["DATABASE_URL"] != "postgresql://lifecycle-secret" || bound["REDIS_URL"] != "rediss://:redis-secret@fixture.upstash.io:6379" {
				t.Fatal("outputs did not reach approved GitHub destination")
			}
			retainCache = true
			if err := engine.Destroy(context.Background(), b, &env, ""); err == nil || env.State != "RESIDUAL" || len(env.Residuals) != 1 || env.Residuals[0] != "cache" {
				t.Fatal("unverified deletion was not retained as residual")
			}
			if len(deleted) != 2 || deleted[0] != "cache" || deleted[1] != "db" || ids["shared"] != "redis-shared" {
				t.Fatal("destroy violated ordering or referenced ownership")
			}
			retainCache = false
			if err := engine.Destroy(context.Background(), b, &env, ""); err != nil || env.State != "DESTROYED" || len(env.Resources) != 0 || len(env.Residuals) != 0 || env.LastError != "" || env.FailureCode != "" {
				t.Fatal("residual retry did not verify absence")
			}
			if err := engine.Destroy(context.Background(), b, &env, ""); err != nil || len(deleted) != 3 {
				t.Fatal("destroy retry was not idempotent")
			}
		})
	}
}

func TestProviderDiagnosticsReachEngine(t *testing.T) {
	const leak = "BODY-MARKER postgresql://role:secret@ep.neon.tech/db https://console.neon.tech/api quota exceeded password-secret"
	t.Run("neon auth before mutation", func(t *testing.T) {
		t.Setenv("NEON_API_KEY", "napi-fixture-key")
		calls := 0
		withProviderTransport(t, func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Method != "GET" || r.URL.Host != "console.neon.tech" {
				t.Fatalf("mutated before diagnosis: %s %s", r.Method, r.URL)
			}
			return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(leak)), Header: make(http.Header)}, nil
		})
		err, env := applyProvider(t, "db", "neon@1", map[string]string{"project_id": "proj-fixture", "database_name": "neondb", "role_name": "owner"}, func(ctx context.Context, c core.Component, r adapter.Request) (adapter.Response, error) {
			if r.Hook == "manifest" {
				m := neon.Handler{}.Manifest()
				return adapter.Response{Manifest: &m}, nil
			}
			return neon.Handler{}.Handle(ctx, r)
		})
		if calls != 1 {
			t.Fatalf("requests=%d", calls)
		}
		assertProviderFailure(t, err, env, "db", "read", "neon_unauthorized", "FAILED", leak)
	})
	t.Run("neon missing credential", func(t *testing.T) {
		t.Setenv("NEON_API_KEY", "")
		withProviderTransport(t, func(r *http.Request) (*http.Response, error) {
			t.Fatalf("request before credential check: %s %s", r.Method, r.URL)
			return nil, nil
		})
		err, env := applyProvider(t, "db", "neon@1", map[string]string{"project_id": "proj-fixture"}, func(ctx context.Context, c core.Component, r adapter.Request) (adapter.Response, error) {
			if r.Hook == "manifest" {
				m := neon.Handler{}.Manifest()
				return adapter.Response{Manifest: &m}, nil
			}
			return neon.Handler{}.Handle(ctx, r)
		})
		assertProviderFailure(t, err, env, "db", "read", "neon_credential_missing", "FAILED", leak)
	})
	t.Run("upstash quota keeps live create", func(t *testing.T) {
		t.Setenv("UPSTASH_EMAIL", "person@example.com")
		t.Setenv("UPSTASH_API_KEY", "upstash-fixture-key")
		prev := retryBackoff
		retryBackoff = 0
		t.Cleanup(func() { retryBackoff = prev })
		posts := 0
		withProviderTransport(t, func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "api.upstash.com" {
				t.Fatalf("unexpected host %s", r.URL.Host)
			}
			if r.Method == "POST" {
				posts++
				var posted struct {
					DatabaseName  string `json:"database_name"`
					Platform      string `json:"platform"`
					PrimaryRegion string `json:"primary_region"`
				}
				dec := json.NewDecoder(r.Body)
				dec.DisallowUnknownFields()
				if err := dec.Decode(&posted); err != nil || posted.DatabaseName != "preview-cache" || posted.Platform != "aws" || posted.PrimaryRegion != "us-east-1" {
					t.Fatalf("live request shape: %+v %v", posted, err)
				}
				return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`"You cannot have more than one database without a payment method. ` + leak + `"`)), Header: make(http.Header)}, nil
			}
			if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/databases") {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("[]")), Header: make(http.Header)}, nil
			}
			t.Fatalf("unexpected %s %s", r.Method, r.URL)
			return nil, nil
		})
		err, env := applyProvider(t, "cache", "upstash@1", map[string]string{}, func(ctx context.Context, c core.Component, r adapter.Request) (adapter.Response, error) {
			if r.Hook == "manifest" {
				m := upstash.Handler{}.Manifest()
				return adapter.Response{Manifest: &m}, nil
			}
			return upstash.Handler{}.Handle(ctx, r)
		})
		if posts != 3 {
			t.Fatalf("creates=%d", posts)
		}
		assertProviderFailure(t, err, env, "cache", "apply", "upstash_quota", "DEGRADED", leak)
	})
}

func withProviderTransport(t *testing.T, rt lifecycleTransport) {
	t.Helper()
	old := httpclient.Client
	t.Cleanup(func() { httpclient.Client = old })
	httpclient.Client = &http.Client{Transport: rt}
}

func applyProvider(t *testing.T, id, adapterName string, config map[string]string, call CallFunc) (error, core.Environment) {
	t.Helper()
	component := core.Component{ID: id, Adapter: adapterName, Executable: "fixture", SHA256: strings.Repeat("a", 64), Ownership: core.Managed, Config: config}
	b := core.Blueprint{Name: "preview", Components: []core.Component{component}}
	plan, err := core.NewPlan(b, "preview", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	env := core.Environment{Name: "preview", State: "PLANNED", Resources: map[string]core.Resource{}}
	return Engine{Call: call}.Apply(context.Background(), plan, &env), env
}

func assertProviderFailure(t *testing.T, err error, env core.Environment, component, hook, code, state, leak string) {
	t.Helper()
	var failure *adapter.Failure
	var coded *adapter.CodedError
	if !errors.As(err, &failure) || failure.Code != code || failure.Component != component || failure.Hook != hook {
		t.Fatalf("%v", err)
	}
	if !errors.As(err, &coded) || coded.Code != code || env.State != state || env.LastError != err.Error() || env.FailureCode != code || env.FailureComponent != component || env.FailurePhase != hook {
		t.Fatalf("state=%s last=%q err=%v", env.State, env.LastError, err)
	}
	serialized, _ := json.Marshal(env)
	for _, text := range []string{err.Error(), env.LastError, string(serialized)} {
		for _, marker := range []string{"BODY-MARKER", "postgresql://", "https://", "quota exceeded", "password-secret", "napi-fixture-key", "upstash-fixture-key", "person@example.com", leak} {
			if strings.Contains(text, marker) {
				t.Fatalf("diagnostic leaked %q in %s", marker, text)
			}
		}
	}
}
