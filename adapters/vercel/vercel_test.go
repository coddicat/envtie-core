package vercel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coddicat/envtie-core/adapters/httpclient"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func withAPI(t *testing.T, fn roundTrip) {
	t.Helper()
	oldClient, oldBase, oldPoll, oldTimeout := httpclient.Client, apiBase, pollInterval, pollTimeout
	t.Cleanup(func() {
		httpclient.Client = oldClient
		apiBase = oldBase
		pollInterval = oldPoll
		pollTimeout = oldTimeout
	})
	apiBase = "https://vercel.test"
	pollInterval = time.Millisecond
	pollTimeout = time.Second
	httpclient.Client = &http.Client{Transport: fn}
}

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestApplyUpsertsBranchEnvDeploysAndReturnsURL(t *testing.T) {
	t.Setenv("VERCEL_TOKEN", "vt_fixture")
	t.Setenv("GITHUB_HEAD_REF", "")
	created := false
	ready := false
	deletedPrev := false
	withAPI(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer vt_fixture" {
			t.Errorf("auth %s", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v9/projects/prj_app"):
			return jsonResp(200, `{"id":"prj_app","name":"app","link":{"type":"github","repoId":99,"repo":"app","org":"acme"}}`), nil
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/env"):
			if r.URL.Query().Get("upsert") != "true" {
				t.Fatal("expected upsert")
			}
			raw, _ := io.ReadAll(r.Body)
			var items []map[string]any
			if err := json.Unmarshal(raw, &items); err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || items[0]["key"] != "DATABASE_URL" || items[0]["gitBranch"] != "pr-7" {
				t.Fatalf("env body %s", raw)
			}
			targets, _ := items[0]["target"].([]any)
			if len(targets) != 1 || targets[0] != "preview" {
				t.Fatalf("target %#v", items[0]["target"])
			}
			if _, ok := items[0]["customEnvironmentIds"]; ok {
				t.Fatal("must not use custom environments")
			}
			return jsonResp(201, `{"failed":[]}`), nil
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v13/deployments"):
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			gs, _ := body["gitSource"].(map[string]any)
			if body["target"] != "preview" || gs["ref"] != "pr-7" || gs["type"] != "github" {
				t.Fatalf("deploy body %s", raw)
			}
			created = true
			return jsonResp(200, `{"id":"dpl_2","url":"pr-7-app.vercel.app","readyState":"BUILDING"}`), nil
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v13/deployments/dpl_2"):
			if !created {
				t.Fatal("poll before create")
			}
			if !ready {
				ready = true
				return jsonResp(200, `{"id":"dpl_2","url":"pr-7-app.vercel.app","readyState":"BUILDING","meta":{"githubCommitRef":"pr-7"}}`), nil
			}
			return jsonResp(200, `{"id":"dpl_2","url":"pr-7-app.vercel.app","readyState":"READY","meta":{"githubCommitRef":"pr-7"}}`), nil
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/aliases"):
			return jsonResp(200, `{"aliases":[{"alias":"test-consumer-envtie-git-pr-7-acme.vercel.app"}]}`), nil
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/v13/deployments/dpl_1"):
			deletedPrev = true
			return jsonResp(200, `{}`), nil
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.String())
			return jsonResp(404, `{}`), nil
		}
	})
	resp, err := Handler{}.Handle(context.Background(), adapter.Request{
		Hook: "apply", Environment: "pr-7", Component: "host", ExternalID: "dpl_1",
		Config: map[string]string{"project_id": "prj_app", "env.DATABASE_URL": "postgres://x"},
	})
	if err != nil || resp.ExternalID != "dpl_2" || resp.Outputs["VERCEL_URL"] != "https://test-consumer-envtie-git-pr-7-acme.vercel.app" || !deletedPrev {
		t.Fatalf("%+v err=%v deletedPrev=%v", resp, err, deletedPrev)
	}
}

func TestDestroyRemovesBranchEnvAndDeployments(t *testing.T) {
	t.Setenv("VERCEL_TOKEN", "vt_fixture")
	deletedEnv, deletedTracked, deletedListed := false, false, false
	listCalls := 0
	withAPI(t, func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v9/projects/prj_app") && !strings.Contains(r.URL.Path, "/env"):
			return jsonResp(200, `{"id":"prj_app","name":"app","link":{"type":"github","repoId":99}}`), nil
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v10/projects/prj_app/env"):
			if r.URL.Query().Get("gitBranch") != "pr-7" {
				t.Fatalf("gitBranch %s", r.URL.Query().Get("gitBranch"))
			}
			listCalls++
			if listCalls > 1 {
				return jsonResp(200, `{"envs":[]}`), nil
			}
			return jsonResp(200, `{"envs":[{"id":"env_1","key":"DATABASE_URL","gitBranch":"pr-7","target":["preview"]}]}`), nil
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/env/env_1"):
			deletedEnv = true
			return jsonResp(200, `{}`), nil
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v6/deployments"):
			if r.URL.Query().Get("branch") != "pr-7" {
				t.Fatalf("branch %s", r.URL.Query().Get("branch"))
			}
			if deletedListed {
				return jsonResp(200, `{"deployments":[]}`), nil
			}
			return jsonResp(200, `{"deployments":[{"id":"dpl_2"}]}`), nil
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/v13/deployments/dpl_1"):
			deletedTracked = true
			return jsonResp(200, `{}`), nil
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/v13/deployments/dpl_2"):
			deletedListed = true
			return jsonResp(200, `{}`), nil
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.String())
			return jsonResp(404, `{}`), nil
		}
	})
	resp, err := Handler{}.Handle(context.Background(), adapter.Request{
		Hook: "destroy", Environment: "pr-7", Component: "host",
		Config: map[string]string{"project_id": "prj_app"}, ExternalID: "dpl_1",
	})
	if err != nil || !resp.Absent || !deletedEnv || !deletedTracked || !deletedListed {
		t.Fatalf("%+v err=%v env=%v tracked=%v listed=%v", resp, err, deletedEnv, deletedTracked, deletedListed)
	}
}

func TestDestroyWithoutExternalIDCleansBranch(t *testing.T) {
	t.Setenv("VERCEL_TOKEN", "vt")
	deletedEnv := false
	withAPI(t, func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v9/projects/"):
			return jsonResp(200, `{"id":"prj","name":"app","link":{"type":"github","repoId":1}}`), nil
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/env"):
			if deletedEnv {
				return jsonResp(200, `{"envs":[]}`), nil
			}
			return jsonResp(200, `{"envs":[{"id":"e1","key":"X","gitBranch":"pr-7","target":["preview"]}]}`), nil
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/env/e1"):
			deletedEnv = true
			return jsonResp(200, `{}`), nil
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v6/deployments"):
			return jsonResp(200, `{"deployments":[]}`), nil
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.String())
			return jsonResp(404, `{}`), nil
		}
	})
	resp, err := Handler{}.Handle(context.Background(), adapter.Request{
		Hook: "destroy", Environment: "pr-7", Config: map[string]string{"project_id": "prj"},
	})
	if err != nil || !resp.Absent || !deletedEnv {
		t.Fatalf("%+v err=%v deleted=%v", resp, err, deletedEnv)
	}
}

func TestReadWrongBranchIsAbsent(t *testing.T) {
	t.Setenv("VERCEL_TOKEN", "vt")
	withAPI(t, func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"id":"dpl_1","url":"x.vercel.app","readyState":"READY","meta":{"githubCommitRef":"other"}}`), nil
	})
	resp, err := Handler{}.Handle(context.Background(), adapter.Request{
		Hook: "read", Environment: "pr-7", ExternalID: "dpl_1",
		Config: map[string]string{"project_id": "prj"},
	})
	if err != nil || !resp.Absent {
		t.Fatalf("%+v %v", resp, err)
	}
}

func TestMissingTokenAndUnlinkedGit(t *testing.T) {
	t.Setenv("VERCEL_TOKEN", "")
	_, err := Handler{}.Handle(context.Background(), adapter.Request{
		Hook: "apply", Environment: "pr-7", Config: map[string]string{"project_id": "prj"},
	})
	if err == nil || !strings.Contains(err.Error(), "vercel_credential_missing") {
		t.Fatalf("%v", err)
	}
	t.Setenv("VERCEL_TOKEN", "vt")
	withAPI(t, func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"id":"prj","name":"app"}`), nil
	})
	_, err = Handler{}.Handle(context.Background(), adapter.Request{
		Hook: "apply", Environment: "pr-7", Config: map[string]string{"project_id": "prj"},
	})
	if err == nil || !strings.Contains(err.Error(), "vercel_git_unlinked") {
		t.Fatalf("%v", err)
	}
}

func TestManifest(t *testing.T) {
	m := Handler{}.Manifest()
	if m.Name != "vercel" || m.CredentialEnv[0] != "VERCEL_TOKEN" {
		t.Fatalf("%+v", m)
	}
	names := map[string]bool{}
	for _, name := range m.EnvNames() {
		names[name] = true
	}
	if !names["GITHUB_HEAD_REF"] || !names["GITHUB_SHA"] {
		t.Fatalf("expected Actions context in EnvNames, got %+v", m.EnvNames())
	}
}

func TestApplyUsesActionsBranchAndCommit(t *testing.T) {
	t.Setenv("VERCEL_TOKEN", "vt_fixture")
	t.Setenv("GITHUB_HEAD_REF", "feature/preview")
	t.Setenv("GITHUB_SHA", "abc123def456")
	t.Cleanup(func() {
		t.Setenv("GITHUB_HEAD_REF", "")
		t.Setenv("GITHUB_SHA", "")
	})
	withAPI(t, func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v9/projects/prj_app"):
			return jsonResp(200, `{"id":"prj_app","name":"app","link":{"type":"github","repoId":99,"repo":"app","org":"acme"}}`), nil
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/env"):
			raw, _ := io.ReadAll(r.Body)
			var items []map[string]any
			if err := json.Unmarshal(raw, &items); err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || items[0]["gitBranch"] != "feature/preview" {
				t.Fatalf("env body %s", raw)
			}
			return jsonResp(201, `{"failed":[]}`), nil
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v13/deployments"):
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			gs, _ := body["gitSource"].(map[string]any)
			if gs["ref"] != "abc123def456" {
				t.Fatalf("expected commit ref, got %s", raw)
			}
			return jsonResp(200, `{"id":"dpl_sha","url":"feat.vercel.app","readyState":"BUILDING"}`), nil
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v13/deployments/dpl_sha"):
			return jsonResp(200, `{"id":"dpl_sha","url":"feat.vercel.app","readyState":"READY","meta":{"githubCommitRef":"feature/preview"}}`), nil
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/aliases"):
			return jsonResp(200, `{"aliases":[]}`), nil
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.String())
			return jsonResp(404, `{}`), nil
		}
	})
	resp, err := Handler{}.Handle(context.Background(), adapter.Request{
		Hook: "apply", Environment: "pr-27", Component: "host",
		Config: map[string]string{"project_id": "prj_app", "env.DATABASE_URL": "postgres://x"},
	})
	if err != nil || resp.Outputs["VERCEL_GIT_BRANCH"] != "feature/preview" {
		t.Fatalf("%+v %v", resp, err)
	}
}
