package github

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/coddicat/envtie-core/adapters/httpclient"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

func TestEnvironmentLifecycle(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "installation-token")
	t.Setenv("GITHUB_SHA", "abc123")
	oldClient := httpclient.Client
	oldAPI := githubAPI
	t.Cleanup(func() {
		httpclient.Client = oldClient
		githubAPI = oldAPI
	})
	githubAPI = "https://github.test"
	managed := false
	httpclient.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (int, string) {
		if r.Header.Get("Authorization") != "Bearer installation-token" {
			t.Errorf("auth %s", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/environments/pr-13"):
			if !managed {
				return http.StatusNotFound, `{"message":"Not Found"}`
			}
			return http.StatusOK, `{"name":"pr-13"}`
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/environments/pr-13"):
			managed = true
			return http.StatusOK, `{"name":"pr-13"}`
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/variables"):
			if managed {
				return http.StatusOK, `{"variables":[{"name":"ENVTIE_MANAGED","value":"true"}]}`
			}
			return http.StatusOK, `{"variables":[]}`
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/variables/"):
			return http.StatusNotFound, `{}`
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/variables"):
			return http.StatusCreated, `{}`
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/deployments"):
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"transient_environment":true`) || strings.Contains(string(body), "secret") {
				t.Errorf("deployment body %s", body)
			}
			return http.StatusCreated, `{"id":7024821214}`
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/deployments/7024821214/statuses"):
			return http.StatusCreated, `{"id":1}`
		case r.Method == http.MethodDelete:
			managed = false
			return http.StatusNoContent, ""
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return http.StatusNotFound, `{}`
		}
	})}
	req := adapter.Request{Hook: "apply", Environment: "pr-13", Config: map[string]string{"repository": "acme/app", "deployment": "true", "environment_url": "https://pr-13.example.com"}}
	resp, err := (Handler{}).Handle(context.Background(), req)
	if err != nil || resp.ExternalID != "pr-13" {
		t.Fatalf("apply %+v %v", resp, err)
	}
	req.Hook = "read"
	resp, err = (Handler{}).Handle(context.Background(), req)
	if err != nil || resp.Absent {
		t.Fatalf("read %+v %v", resp, err)
	}
	req.Hook = "destroy"
	resp, err = (Handler{}).Handle(context.Background(), req)
	if err != nil || !resp.Absent {
		t.Fatalf("destroy %+v %v", resp, err)
	}
}

func TestManagedResourceNameDoesNotCreateASecondEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "installation-token")
	oldClient := httpclient.Client
	oldAPI := githubAPI
	t.Cleanup(func() {
		httpclient.Client = oldClient
		githubAPI = oldAPI
	})
	githubAPI = "https://github.test"
	httpclient.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (int, string) {
		if strings.Contains(r.URL.Path, "pr-13-github") {
			t.Errorf("second environment %s %s", r.Method, r.URL.Path)
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/environments/pr-13") {
			return http.StatusNotFound, `{"message":"Not Found"}`
		}
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/environments/pr-13") {
			return http.StatusOK, `{"name":"pr-13"}`
		}
		if strings.Contains(r.URL.Path, "/variables") {
			return http.StatusCreated, `{}`
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		return http.StatusNotFound, `{}`
	})}
	resp, err := (Handler{}).Handle(context.Background(), adapter.Request{
		Hook: "apply", Environment: "pr-13",
		Config: map[string]string{"repository": "acme/app", "name": "pr-13-github"},
	})
	if err != nil || resp.ExternalID != "pr-13" {
		t.Fatalf("apply %+v %v", resp, err)
	}
}

func TestForeignEnvironmentIsRejected(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "installation-token")
	oldClient := httpclient.Client
	oldAPI := githubAPI
	t.Cleanup(func() {
		httpclient.Client = oldClient
		githubAPI = oldAPI
	})
	githubAPI = "https://github.test"
	httpclient.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (int, string) {
		if strings.Contains(r.URL.Path, "/variables") {
			return http.StatusOK, `{"variables":[{"name":"OTHER","value":"1"}]}`
		}
		return http.StatusOK, `{"name":"pr-4"}`
	})}
	_, err := (Handler{}).Handle(context.Background(), adapter.Request{Hook: "apply", Environment: "pr-4", Config: map[string]string{"repository": "acme/app"}})
	if err == nil || !strings.Contains(err.Error(), "github_environment_unmanaged") {
		t.Fatal(err)
	}
}

func TestManifestForwardsActionsContext(t *testing.T) {
	names := map[string]bool{}
	for _, name := range (Handler{}).Manifest().CredentialEnv {
		names[name] = true
	}
	for _, name := range []string{"GITHUB_TOKEN", "GITHUB_REPOSITORY", "GITHUB_SHA"} {
		if !names[name] {
			t.Fatalf("adapter process does not receive %s", name)
		}
	}
}

func TestPullRequestReadStaysReferenced(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "installation-token")
	oldClient := httpclient.Client
	oldAPI := githubAPI
	t.Cleanup(func() {
		httpclient.Client = oldClient
		githubAPI = oldAPI
	})
	githubAPI = "https://github.test"
	httpclient.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (int, string) {
		if !strings.Contains(r.URL.Path, "/pulls/9") {
			t.Errorf("path %s", r.URL.Path)
		}
		return http.StatusOK, `{"number":9,"head":{"sha":"def"}}`
	})}
	resp, err := (Handler{}).Handle(context.Background(), adapter.Request{Hook: "outputs", Config: map[string]string{"repository": "acme/app", "pull_number": "9"}})
	if err != nil || resp.Outputs["PR_NUMBER"] != "9" || resp.Outputs["PR_HEAD_SHA"] != "def" {
		t.Fatalf("%+v %v", resp, err)
	}
}

type roundTrip func(*http.Request) (int, string)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	status, body := f(r)
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
}
