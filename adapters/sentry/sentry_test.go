package sentry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/coddicat/envtie-core/adapters/httpclient"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

func TestProjectSlugSanitizesManagedNames(t *testing.T) {
	if got := projectSlug("pr-12-sentry"); got != "pr-12-sentry" {
		t.Fatalf("got %q", got)
	}
	if got := projectSlug("PR_12.Sentry!"); got != "pr-12-sentry" {
		t.Fatalf("got %q", got)
	}
}

func TestLifecycleCreatesProjectAndReturnsDSN(t *testing.T) {
	t.Setenv("SENTRY_AUTH_TOKEN", "sntrys_fixture")
	oldClient := httpclient.Client
	oldBase := apiBase
	t.Cleanup(func() {
		httpclient.Client = oldClient
		apiBase = oldBase
	})
	apiBase = "https://sentry.test/api/0"
	created := false
	httpclient.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (int, string) {
		if r.Header.Get("Authorization") != "Bearer sntrys_fixture" {
			t.Errorf("auth %s", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/acme/pr-7-sentry/"):
			if !created {
				return http.StatusNotFound, `{"detail":"Not found"}`
			}
			return http.StatusOK, `{"id":"1","slug":"pr-7-sentry","name":"pr-7-sentry"}`
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/teams/acme/engineering/projects/"):
			body, _ := io.ReadAll(r.Body)
			var in map[string]any
			_ = json.Unmarshal(body, &in)
			if in["name"] != "pr-7-sentry" || in["slug"] != "pr-7-sentry" || in["platform"] != "node-express" {
				t.Errorf("create body %s", body)
			}
			created = true
			return http.StatusCreated, `{"id":"1","slug":"pr-7-sentry","name":"pr-7-sentry"}`
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/keys/"):
			return http.StatusOK, `[{"id":"k1","dsn":{"public":"https://key@o1.ingest.sentry.io/1"}}]`
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/projects/acme/pr-7-sentry/"):
			created = false
			return http.StatusNoContent, ""
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return http.StatusNotFound, `{}`
		}
	})}
	cfg := map[string]string{
		"organization": "acme", "team": "engineering", "name": "pr-7-sentry", "platform": "node-express",
	}
	resp, err := (Handler{}).Handle(context.Background(), adapter.Request{Hook: "apply", Environment: "pr-7", Component: "sentry", Config: cfg})
	if err != nil || resp.ExternalID != "pr-7-sentry" || resp.Outputs["SENTRY_DSN"] == "" || resp.Outputs["SENTRY_PROJECT_SLUG"] != "pr-7-sentry" {
		t.Fatalf("apply %+v %v", resp, err)
	}
	resp, err = (Handler{}).Handle(context.Background(), adapter.Request{Hook: "read", Environment: "pr-7", Component: "sentry", Config: cfg, ExternalID: "pr-7-sentry"})
	if err != nil || resp.Absent || resp.ExternalID != "pr-7-sentry" {
		t.Fatalf("read %+v %v", resp, err)
	}
	resp, err = (Handler{}).Handle(context.Background(), adapter.Request{Hook: "destroy", Environment: "pr-7", Component: "sentry", Config: cfg, ExternalID: "pr-7-sentry"})
	if err != nil || !resp.Absent {
		t.Fatalf("destroy %+v %v", resp, err)
	}
}

func TestDestroyRefusesUnmanagedProject(t *testing.T) {
	t.Setenv("SENTRY_AUTH_TOKEN", "sntrys_fixture")
	oldClient := httpclient.Client
	oldBase := apiBase
	t.Cleanup(func() {
		httpclient.Client = oldClient
		apiBase = oldBase
	})
	apiBase = "https://sentry.test/api/0"
	httpclient.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (int, string) {
		return http.StatusOK, `{"id":"1","slug":"pr-7-sentry","name":"production-app"}`
	})}
	_, err := (Handler{}).Handle(context.Background(), adapter.Request{
		Hook: "destroy", Environment: "pr-7", Component: "sentry",
		Config: map[string]string{"organization": "acme", "team": "engineering", "name": "pr-7-sentry"}, ExternalID: "pr-7-sentry",
	})
	if err == nil || !strings.Contains(err.Error(), "sentry_project_unmanaged") {
		t.Fatal(err)
	}
}

func TestReadTreatsPendingDeletionAsAbsent(t *testing.T) {
	t.Setenv("SENTRY_AUTH_TOKEN", "sntrys_fixture")
	oldClient := httpclient.Client
	oldBase := apiBase
	t.Cleanup(func() {
		httpclient.Client = oldClient
		apiBase = oldBase
	})
	apiBase = "https://sentry.test/api/0"
	httpclient.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (int, string) {
		return http.StatusOK, `{"id":"1","slug":"pr-22-sentry","name":"pr-22-sentry","status":"pending_deletion"}`
	})}
	resp, err := (Handler{}).Handle(context.Background(), adapter.Request{
		Hook: "read", Environment: "pr-22", Component: "sentry",
		Config: map[string]string{"organization": "solomonyk-llc", "team": "engineering", "name": "pr-22-sentry"}, ExternalID: "pr-22-sentry",
	})
	if err != nil || !resp.Absent {
		t.Fatalf("pending deletion should be absent: %+v %v", resp, err)
	}
}

func TestDestroyThenReadPendingDeletionClears(t *testing.T) {
	t.Setenv("SENTRY_AUTH_TOKEN", "sntrys_fixture")
	oldClient := httpclient.Client
	oldBase := apiBase
	t.Cleanup(func() {
		httpclient.Client = oldClient
		apiBase = oldBase
	})
	apiBase = "https://sentry.test/api/0"
	phase := "active"
	httpclient.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (int, string) {
		switch {
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/projects/solomonyk-llc/pr-22-sentry/"):
			phase = "pending_deletion"
			return http.StatusNoContent, ""
		case r.Method == http.MethodGet:
			return http.StatusOK, `{"id":"1","slug":"pr-22-sentry","name":"pr-22-sentry","status":"` + phase + `"}`
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return http.StatusNotFound, `{}`
		}
	})}
	req := adapter.Request{
		Hook: "destroy", Environment: "pr-22", Component: "sentry",
		Config: map[string]string{"organization": "solomonyk-llc", "team": "engineering", "name": "pr-22-sentry"}, ExternalID: "pr-22-sentry",
	}
	if resp, err := (Handler{}).Handle(context.Background(), req); err != nil || !resp.Absent {
		t.Fatalf("204 destroy should be absent: %+v %v", resp, err)
	}
	req.Hook = "read"
	if resp, err := (Handler{}).Handle(context.Background(), req); err != nil || !resp.Absent {
		t.Fatalf("post-destroy pending_deletion read should be absent: %+v %v", resp, err)
	}
}

func TestMissingTokenFailsBeforeProviderCall(t *testing.T) {
	t.Setenv("SENTRY_AUTH_TOKEN", "")
	calls := 0
	oldClient := httpclient.Client
	t.Cleanup(func() { httpclient.Client = oldClient })
	httpclient.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (int, string) {
		calls++
		return http.StatusOK, `{}`
	})}
	_, err := (Handler{}).Handle(context.Background(), adapter.Request{
		Hook: "apply", Config: map[string]string{"organization": "acme", "team": "engineering", "name": "pr-1-sentry"},
	})
	if err == nil || !strings.Contains(err.Error(), "sentry_credential_missing") || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

type roundTrip func(*http.Request) (int, string)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	status, body := f(r)
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
}
