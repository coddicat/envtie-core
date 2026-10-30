package upstash

import (
	"context"
	"encoding/json"
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

func TestDatabaseLifecycleContract(t *testing.T) {
	t.Setenv("UPSTASH_EMAIL", "test@example.com")
	t.Setenv("UPSTASH_API_KEY", "test-key")
	old := httpclient.Client
	defer func() { httpclient.Client = old }()
	httpclient.Client = &http.Client{Transport: fakeTransport(func(r *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
			t.Fatal("missing Upstash auth")
		}
		status, body := 200, "{}"
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/databases"):
			body = `[]`
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/database"):
			assertLiveCreateBody(t, r, "pr-1-cache", "aws", "us-east-1")
			body = `{"database_id":"safe-id-1"}`
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/database/safe-id-1"):
			body = `{"database_id":"safe-id-1","endpoint":"example-123","password":"secret-fixture","rest_token":"rest-secret"}`
		case r.Method == "DELETE" && strings.HasSuffix(r.URL.Path, "/database/safe-id-1"):
			body = `"OK"`
		default:
			t.Fatalf("unexpected Upstash request: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	h := Handler{}
	base := adapter.Request{Config: map[string]string{"name": "pr-1-cache", "platform": "aws", "primary_region": "us-east-1"}}
	base.Hook = "read"
	read, err := h.Handle(context.Background(), base)
	if err != nil || !read.Absent {
		t.Fatalf("read: %+v %v", read, err)
	}
	base.Hook = "apply"
	created, err := h.Handle(context.Background(), base)
	if err != nil || created.ExternalID != "safe-id-1" {
		t.Fatalf("apply: %+v %v", created, err)
	}
	base.ExternalID = created.ExternalID
	base.Hook = "outputs"
	out, err := h.Handle(context.Background(), base)
	if err != nil || out.Outputs["REDIS_URL"] != "rediss://:secret-fixture@example-123.upstash.io:6379" || out.Outputs["UPSTASH_REDIS_REST_TOKEN"] != "rest-secret" {
		t.Fatalf("outputs: %+v %v", out, err)
	}
	base.Hook = "destroy"
	gone, err := h.Handle(context.Background(), base)
	if err != nil || !gone.Absent {
		t.Fatalf("destroy: %+v %v", gone, err)
	}
}

func TestDiagnostics(t *testing.T) {
	const leak = `BODY-MARKER endpoint incompatibility https://api.upstash.com/v2/redis/database postgresql://:password-secret@example.upstash.io quota exceeded`
	full := map[string]string{"name": "pr-1-cache", "platform": "aws", "primary_region": "us-east-1"}
	cases := []struct {
		name       string
		hook       string
		external   string
		email      string
		key        string
		config     map[string]string
		status     int
		body       string
		transport  bool
		want       string
		wantAbsent bool
		requests   bool
		method     string
		create     bool
	}{
		{name: "missing email", hook: "apply", key: "upstash-fixture-key", config: full, want: "upstash_credential_missing"},
		{name: "missing key", hook: "read", email: "person@example.com", config: full, want: "upstash_credential_missing"},
		{name: "validate missing credential", hook: "validate", config: full, want: "upstash_credential_missing"},
		{name: "missing name", hook: "apply", email: "person@example.com", key: "upstash-fixture-key", config: map[string]string{"platform": "aws"}, want: "upstash_name_required"},
		{name: "read missing name", hook: "read", email: "person@example.com", key: "upstash-fixture-key", config: map[string]string{}, want: "upstash_name_required"},
		{name: "outputs without id", hook: "outputs", email: "person@example.com", key: "upstash-fixture-key", config: full, want: "upstash_outputs_unavailable"},
		{name: "destroy without id", hook: "destroy", email: "person@example.com", key: "upstash-fixture-key", config: full, want: "upstash_invalid_request"},
		{name: "destroy absent", hook: "destroy", email: "person@example.com", key: "upstash-fixture-key", config: full, external: "db-1", status: 404, wantAbsent: true, requests: true, method: "DELETE"},
		{name: "unauthorized", hook: "read", email: "person@example.com", key: "upstash-fixture-key", config: full, status: 401, want: "upstash_unauthorized", requests: true, method: "GET"},
		{name: "forbidden", hook: "apply", email: "person@example.com", key: "upstash-fixture-key", config: full, status: 403, want: "upstash_unauthorized", requests: true, method: "POST", create: true},
		{name: "not found", hook: "read", email: "person@example.com", key: "upstash-fixture-key", config: full, status: 404, want: "upstash_not_found", requests: true, method: "GET"},
		{name: "database absent", hook: "read", email: "person@example.com", key: "upstash-fixture-key", config: full, external: "db-missing", status: 404, wantAbsent: true, requests: true, method: "GET"},
		{name: "quota is not endpoint incompatibility", hook: "apply", email: "person@example.com", key: "upstash-fixture-key", config: full, status: 400, body: `"You cannot have more than 1 database(s). You can add a payment method to create more in pay as you go plan."`, want: "upstash_quota", requests: true, method: "POST", create: true},
		{name: "bad create request is not quota", hook: "apply", email: "person@example.com", key: "upstash-fixture-key", config: full, status: 400, body: `"primary_region is invalid"`, want: "upstash_invalid_request", requests: true, method: "POST", create: true},
		{name: "payment required", hook: "apply", email: "person@example.com", key: "upstash-fixture-key", config: full, status: 402, want: "upstash_quota", requests: true, method: "POST", create: true},
		{name: "rate limit", hook: "read", email: "person@example.com", key: "upstash-fixture-key", config: full, status: 429, want: "upstash_rate_limited", requests: true, method: "GET"},
		{name: "invalid request", hook: "apply", email: "person@example.com", key: "upstash-fixture-key", config: full, status: 422, want: "upstash_invalid_request", requests: true, method: "POST", create: true},
		{name: "outputs missing", hook: "outputs", email: "person@example.com", key: "upstash-fixture-key", config: full, external: "db-1", status: 404, want: "upstash_outputs_unavailable", requests: true, method: "GET"},
		{name: "outputs empty", hook: "outputs", email: "person@example.com", key: "upstash-fixture-key", config: full, external: "db-1", status: 200, body: `{"endpoint":"https://secret.example","password":"","rest_token":"tok-secret","note":"` + leak + `"}`, want: "upstash_outputs_unavailable", requests: true, method: "GET"},
		{name: "unavailable", hook: "destroy", email: "person@example.com", key: "upstash-fixture-key", config: full, external: "db-1", status: 500, want: "upstash_unavailable", requests: true, method: "DELETE"},
		{name: "transport", hook: "read", email: "person@example.com", key: "upstash-fixture-key", config: full, transport: true, want: "upstash_unavailable", requests: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("UPSTASH_EMAIL", tc.email)
			t.Setenv("UPSTASH_API_KEY", tc.key)
			calls := 0
			method := ""
			useUpstashTransport(t, func(r *http.Request) (*http.Response, error) {
				calls++
				method = r.Method
				if tc.create {
					assertLiveCreateBody(t, r, tc.config["name"], "aws", "us-east-1")
				}
				if tc.transport {
					return nil, errors.New(`Get "https://api.upstash.com/v2/redis/databases": ` + leak)
				}
				body := tc.body
				if body == "" {
					body = leak
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})
			got, err := Handler{}.Handle(context.Background(), adapter.Request{Hook: tc.hook, ExternalID: tc.external, Config: tc.config, Component: "cache", Environment: "preview"})
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

func TestDefaultCreateShape(t *testing.T) {
	t.Setenv("UPSTASH_EMAIL", "person@example.com")
	t.Setenv("UPSTASH_API_KEY", "upstash-fixture-key")
	useUpstashTransport(t, func(r *http.Request) (*http.Response, error) {
		assertLiveCreateBody(t, r, "pr-1-cache", "aws", "us-east-1")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"database_id":"safe-id-1"}`)), Header: make(http.Header)}, nil
	})
	got, err := Handler{}.Handle(context.Background(), adapter.Request{Hook: "apply", Config: map[string]string{"name": "pr-1-cache"}})
	if err != nil || got.ExternalID != "safe-id-1" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestVectorIndexLifecycle(t *testing.T) {
	t.Setenv("UPSTASH_EMAIL", "test@example.com")
	t.Setenv("UPSTASH_API_KEY", "test-key")
	cfg := map[string]string{
		"product":              "vector",
		"name":                 "pr-1-vector",
		"region":               "us-east-1",
		"similarity_function":  "COSINE",
		"dimension_count":      "1536",
		"index_type":           "DENSE",
		"email":                "test@example.com",
	}
	useUpstashTransport(t, func(r *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
			t.Fatal("missing Upstash auth")
		}
		status, body := 200, "{}"
		switch {
		case r.Method == "GET" && r.URL.Path == "/v2/vector/index":
			body = `[]`
		case r.Method == "POST" && r.URL.Path == "/v2/vector/index":
			assertVectorCreateBody(t, r, "pr-1-vector", "us-east-1", "COSINE", 1536, "DENSE")
			body = `{"id":"idx-1","name":"pr-1-vector","endpoint":"glowing-baboon-15797-us1","token":"vector-secret"}`
		case r.Method == "GET" && r.URL.Path == "/v2/vector/index/idx-1":
			body = `{"id":"idx-1","name":"pr-1-vector","endpoint":"glowing-baboon-15797-us1","token":"vector-secret"}`
		case r.Method == "DELETE" && r.URL.Path == "/v2/vector/index/idx-1":
			body = `"OK"`
		default:
			t.Fatalf("unexpected Upstash request: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	h := Handler{}
	base := adapter.Request{Config: cfg}
	base.Hook = "read"
	read, err := h.Handle(context.Background(), base)
	if err != nil || !read.Absent {
		t.Fatalf("read: %+v %v", read, err)
	}
	base.Hook = "apply"
	created, err := h.Handle(context.Background(), base)
	if err != nil || created.ExternalID != "idx-1" {
		t.Fatalf("apply: %+v %v", created, err)
	}
	base.ExternalID = created.ExternalID
	base.Hook = "outputs"
	out, err := h.Handle(context.Background(), base)
	if err != nil ||
		out.Outputs["UPSTASH_VECTOR_REST_URL"] != "https://glowing-baboon-15797-us1.upstash.io" ||
		out.Outputs["UPSTASH_VECTOR_REST_TOKEN"] != "vector-secret" ||
		out.Outputs["REDIS_URL"] != "" {
		t.Fatalf("outputs: %+v %v", out, err)
	}
	base.Hook = "destroy"
	gone, err := h.Handle(context.Background(), base)
	if err != nil || !gone.Absent {
		t.Fatalf("destroy: %+v %v", gone, err)
	}
}

func TestVectorConfigValidation(t *testing.T) {
	t.Setenv("UPSTASH_EMAIL", "person@example.com")
	t.Setenv("UPSTASH_API_KEY", "upstash-fixture-key")
	cases := []struct {
		name   string
		config map[string]string
		want   string
	}{
		{
			name:   "bad product",
			config: map[string]string{"product": "qstash", "name": "x"},
			want:   "upstash_product_invalid",
		},
		{
			name:   "missing dimension",
			config: map[string]string{"product": "vector", "name": "x", "region": "us-east-1"},
			want:   "upstash_dimension_required",
		},
		{
			name:   "bad similarity",
			config: map[string]string{"product": "vector", "name": "x", "dimension_count": "8", "similarity_function": "L2"},
			want:   "upstash_similarity_invalid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Handler{}.Handle(context.Background(), adapter.Request{Hook: "apply", Config: tc.config})
			var coded *adapter.CodedError
			if !errors.As(err, &coded) || coded.Code != tc.want {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
}

func assertVectorCreateBody(t *testing.T, r *http.Request, name, region, similarity string, dim int, indexType string) {
	t.Helper()
	var posted struct {
		Name               string `json:"name"`
		Region             string `json:"region"`
		SimilarityFunction string `json:"similarity_function"`
		DimensionCount     int    `json:"dimension_count"`
		IndexType          string `json:"index_type"`
		Type               string `json:"type"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&posted); err != nil ||
		posted.Name != name ||
		posted.Region != region ||
		posted.SimilarityFunction != similarity ||
		posted.DimensionCount != dim ||
		posted.IndexType != indexType ||
		posted.Type != "payg" {
		t.Fatalf("vector create body: %+v %v", posted, err)
	}
}

func assertLiveCreateBody(t *testing.T, r *http.Request, name, platform, region string) {
	t.Helper()
	var posted struct {
		DatabaseName  string `json:"database_name"`
		Platform      string `json:"platform"`
		PrimaryRegion string `json:"primary_region"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&posted); err != nil || posted.DatabaseName != name || posted.Platform != platform || posted.PrimaryRegion != region {
		t.Fatalf("live request shape: %+v %v", posted, err)
	}
}

func useUpstashTransport(t *testing.T, rt fakeTransport) {
	t.Helper()
	old := httpclient.Client
	t.Cleanup(func() { httpclient.Client = old })
	httpclient.Client = &http.Client{Transport: rt}
}

func TestAccountEmailPrefersTheBlueprint(t *testing.T) {
	t.Setenv("UPSTASH_EMAIL", "")
	t.Setenv("UPSTASH_API_KEY", "upstash-fixture-key")
	req := adapter.Request{Hook: "read", ExternalID: "db-1", Config: map[string]string{"email": "plan@example.com", "name": "cache"}}
	if err := preflight(req); err != nil {
		t.Fatal(err)
	}
	if accountEmail(req) != "plan@example.com" {
		t.Fatal(accountEmail(req))
	}
}

func assertNoLeak(t *testing.T, err error) {
	t.Helper()
	seen := map[error]bool{}
	for err != nil && !seen[err] {
		seen[err] = true
		text := err.Error()
		for _, leak := range []string{"BODY-MARKER", "postgresql://", "https://", "password-secret", "upstash-fixture-key", "person@example.com", "api.upstash.com", "quota exceeded", "endpoint incompatibility", "tok-secret", "secret.example"} {
			if strings.Contains(text, leak) {
				t.Fatalf("error %q contains %q", text, leak)
			}
		}
		err = errors.Unwrap(err)
	}
}
