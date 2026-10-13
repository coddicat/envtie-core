package neon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/coddicat/envtie-core/adapters/httpclient"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

type fakeTransport func(*http.Request) (*http.Response, error)

func (f fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBranchLifecycleContract(t *testing.T) {
	t.Setenv("NEON_API_KEY", "test-key")
	old := httpclient.Client
	defer func() { httpclient.Client = old }()
	httpclient.Client = &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatal("missing Neon auth")
		}
		status, body := 200, "{}"
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/branches"):
			body = `{"branches":[]}`
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/branches"):
			status = 201
			body = `{"branch":{"id":"br-safe-1"}}`
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/branches/br-safe-1"):
			body = `{"branch":{"id":"br-safe-1"}}`
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/connection_uri"):
			body = `{"uri":"postgresql://secret-fixture"}`
		case r.Method == "DELETE" && strings.HasSuffix(r.URL.Path, "/branches/br-safe-1"):
			status = 204
			body = ""
		default:
			t.Fatalf("unexpected Neon request: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	h := Handler{}
	base := adapter.Request{Config: map[string]string{"project_id": "safe-project", "name": "pr-1-db", "database_name": "neondb", "role_name": "owner"}}
	base.Hook = "read"
	read, err := h.Handle(context.Background(), base)
	if err != nil || !read.Absent {
		t.Fatalf("pre-create read: %+v %v", read, err)
	}
	base.Hook = "apply"
	created, err := h.Handle(context.Background(), base)
	if err != nil || created.ExternalID != "br-safe-1" {
		t.Fatalf("apply: %+v %v", created, err)
	}
	base.ExternalID = created.ExternalID
	base.Hook = "outputs"
	out, err := h.Handle(context.Background(), base)
	if err != nil || out.Outputs["DATABASE_URL"] != "postgresql://secret-fixture" {
		t.Fatalf("outputs: %+v %v", out, err)
	}
	base.Hook = "destroy"
	gone, err := h.Handle(context.Background(), base)
	if err != nil || !gone.Absent {
		t.Fatalf("destroy: %+v %v", gone, err)
	}
}

func TestDiagnostics(t *testing.T) {
	const leak = `BODY-MARKER postgresql://role:secret@ep.neon.tech/db https://console.neon.tech/api/v2/projects/proj-fixture quota exceeded`
	full := map[string]string{"project_id": "proj-fixture", "name": "pr-1-db", "database_name": "neondb", "role_name": "owner"}
	cases := []struct {
		name       string
		hook       string
		external   string
		key        string
		config     map[string]string
		status     int
		body       string
		transport  bool
		want       string
		wantAbsent bool
		requests   bool
		method     string
	}{
		{name: "missing credential", hook: "apply", config: full, want: "neon_credential_missing"},
		{name: "validate missing credential", hook: "validate", config: full, want: "neon_credential_missing"},
		{name: "missing project", hook: "read", key: "napi-fixture-key", config: map[string]string{"name": "pr-1-db"}, want: "neon_project_required"},
		{name: "missing name", hook: "apply", key: "napi-fixture-key", config: map[string]string{"project_id": "proj-fixture"}, want: "neon_name_required"},
		{name: "read missing name", hook: "read", key: "napi-fixture-key", config: map[string]string{"project_id": "proj-fixture"}, want: "neon_name_required"},
		{name: "missing database", hook: "outputs", key: "napi-fixture-key", external: "br-1", config: map[string]string{"project_id": "proj-fixture", "role_name": "owner"}, want: "neon_database_role"},
		{name: "missing role", hook: "outputs", key: "napi-fixture-key", external: "br-1", config: map[string]string{"project_id": "proj-fixture", "database_name": "neondb"}, want: "neon_database_role"},
		{name: "outputs without branch", hook: "outputs", key: "napi-fixture-key", config: full, want: "neon_outputs_unavailable"},
		{name: "destroy without branch", hook: "destroy", key: "napi-fixture-key", config: full, want: "neon_invalid_request"},
		{name: "destroy absent", hook: "destroy", key: "napi-fixture-key", config: full, external: "br-1", status: 404, wantAbsent: true, requests: true, method: "DELETE"},
		{name: "unsupported hook", hook: "migrate", key: "napi-fixture-key", config: full, want: "neon_invalid_request"},
		{name: "unauthorized", hook: "read", key: "napi-fixture-key", config: full, status: 401, want: "neon_unauthorized", requests: true, method: "GET"},
		{name: "forbidden", hook: "apply", key: "napi-fixture-key", config: full, status: 403, want: "neon_unauthorized", requests: true, method: "POST"},
		{name: "project not found", hook: "read", key: "napi-fixture-key", config: full, status: 404, want: "neon_project_not_found", requests: true, method: "GET"},
		{name: "branch absent", hook: "read", key: "napi-fixture-key", config: full, external: "br-missing", status: 404, wantAbsent: true, requests: true, method: "GET"},
		{name: "quota", hook: "apply", key: "napi-fixture-key", config: full, status: 402, want: "neon_quota", requests: true, method: "POST"},
		{name: "rate limit", hook: "apply", key: "napi-fixture-key", config: full, status: 429, want: "neon_rate_limited", requests: true, method: "POST"},
		{name: "invalid request", hook: "apply", key: "napi-fixture-key", config: full, status: 400, want: "neon_invalid_request", requests: true, method: "POST"},
		{name: "unprocessable", hook: "apply", key: "napi-fixture-key", config: full, status: 422, want: "neon_invalid_request", requests: true, method: "POST"},
		{name: "database role missing", hook: "outputs", key: "napi-fixture-key", config: full, external: "br-1", status: 404, want: "neon_database_role", requests: true, method: "GET"},
		{name: "connection uri rejected", hook: "outputs", key: "napi-fixture-key", config: full, external: "br-1", status: 400, want: "neon_connection_uri", requests: true, method: "GET"},
		{name: "connection uri unprocessable", hook: "outputs", key: "napi-fixture-key", config: full, external: "br-1", status: 422, want: "neon_connection_uri", requests: true, method: "GET"},
		{name: "empty connection uri", hook: "outputs", key: "napi-fixture-key", config: full, external: "br-1", status: 200, body: `{"uri":"","message":"` + leak + `"}`, want: "neon_outputs_unavailable", requests: true, method: "GET"},
		{name: "unavailable", hook: "read", key: "napi-fixture-key", config: full, status: 503, want: "neon_unavailable", requests: true, method: "GET"},
		{name: "transport", hook: "read", key: "napi-fixture-key", config: full, transport: true, want: "neon_unavailable", requests: true},
		{name: "invalid payload", hook: "read", key: "napi-fixture-key", config: full, status: 200, body: leak, want: "neon_unavailable", requests: true, method: "GET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NEON_API_KEY", tc.key)
			calls := 0
			method := ""
			useNeonTransport(t, func(r *http.Request) (*http.Response, error) {
				calls++
				method = r.Method
				if tc.transport {
					return nil, errors.New(`Get "https://console.neon.tech/api/v2/projects/proj-fixture": ` + leak)
				}
				body := tc.body
				if body == "" {
					body = leak
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})
			req := adapter.Request{Hook: tc.hook, ExternalID: tc.external, Config: tc.config, Component: "db", Environment: "preview"}
			got, err := Handler{}.Handle(context.Background(), req)
			if calls > 0 != tc.requests || (tc.method != "" && method != tc.method) {
				t.Fatalf("requests=%d method=%s", calls, method)
			}
			if tc.wantAbsent {
				if err != nil || !got.Absent {
					t.Fatalf("absent: %+v %v", got, err)
				}
				return
			}
			var coded *adapter.CodedError
			if !errors.As(err, &coded) || coded.Code != tc.want {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
			assertNoLeak(t, err)
		})
	}
}

func useNeonTransport(t *testing.T, rt fakeTransport) {
	t.Helper()
	old := httpclient.Client
	t.Cleanup(func() { httpclient.Client = old })
	httpclient.Client = &http.Client{Transport: rt}
}

func assertNoLeak(t *testing.T, err error) {
	t.Helper()
	seen := map[error]bool{}
	for err != nil && !seen[err] {
		seen[err] = true
		text := err.Error()
		for _, leak := range []string{"BODY-MARKER", "postgresql://", "https://", "napi-fixture-key", "proj-fixture", "console.neon.tech", "quota exceeded", "role:secret", "neondb"} {
			if strings.Contains(text, leak) {
				t.Fatalf("error %q contains %q", text, leak)
			}
		}
		err = errors.Unwrap(err)
	}
}
