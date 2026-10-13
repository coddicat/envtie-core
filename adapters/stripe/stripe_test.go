package stripe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/coddicat/envtie-core/adapters/httpclient"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

type fakeTransport func(*http.Request) (*http.Response, error)

func (f fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func respond(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func useTransport(t *testing.T, f fakeTransport) {
	t.Helper()
	old := httpclient.Client
	t.Cleanup(func() { httpclient.Client = old })
	httpclient.Client = &http.Client{Transport: f}
}

func request(hook string) adapter.Request {
	return adapter.Request{Hook: hook, Environment: "pr-12", Component: "stripe_webhook", IdempotencyKey: "plan-1/stripe_webhook", Config: map[string]string{
		"name": "pr-12-stripe_webhook", "host": "{env}.preview.example.com", "events": "checkout.session.completed, invoice.paid",
	}}
}

func TestWebhookEndpointLifecycleContract(t *testing.T) {
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_fixture")
	deleted := false
	useTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer sk_test_fixture" {
			t.Fatal("missing Stripe auth")
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/webhook_endpoints":
			if r.URL.Query().Get("starting_after") == "" {
				return respond(200, `{"data":[{"id":"we_other","metadata":{"envtie_name":"someone-else"}}],"has_more":true}`), nil
			}
			return respond(200, `{"data":[],"has_more":false}`), nil
		case r.Method == "POST" && r.URL.Path == "/v1/webhook_endpoints":
			if r.Header.Get("Idempotency-Key") != "plan-1/stripe_webhook" {
				t.Fatal("create must send the idempotency key")
			}
			if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Fatal("Stripe v1 expects form encoding")
			}
			b, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(b))
			if form.Get("url") != "https://pr-12.preview.example.com/api/webhooks/stripe" {
				t.Fatalf("url=%q", form.Get("url"))
			}
			if got := form["enabled_events[]"]; len(got) != 2 || got[0] != "checkout.session.completed" || got[1] != "invoice.paid" {
				t.Fatalf("events=%v", got)
			}
			if form.Get("metadata[envtie_name]") != "pr-12-stripe_webhook" || form.Get("metadata[envtie_environment]") != "pr-12" {
				t.Fatalf("metadata=%v", form)
			}
			return respond(200, `{"id":"we_123","secret":"whsec_fixture","status":"enabled"}`), nil
		case r.URL.Path == "/v1/webhook_endpoints/we_123":
			if r.Method == "DELETE" {
				deleted = true
				return respond(200, `{"id":"we_123","deleted":true}`), nil
			}
			if deleted {
				return respond(404, `{"error":{"message":"No such webhook endpoint"}}`), nil
			}
			return respond(200, `{"id":"we_123","status":"enabled"}`), nil
		}
		t.Fatalf("unexpected Stripe request: %s %s", r.Method, r.URL)
		return nil, nil
	})
	h := Handler{}
	ctx := context.Background()
	read, err := h.Handle(ctx, request("read"))
	if err != nil || !read.Absent {
		t.Fatalf("read: %+v %v", read, err)
	}
	created, err := h.Handle(ctx, request("apply"))
	if err != nil || created.ExternalID != "we_123" || created.Outputs["STRIPE_WEBHOOK_SECRET"] != "whsec_fixture" {
		t.Fatalf("apply: %+v %v", created, err)
	}
	req := request("validate")
	req.ExternalID = created.ExternalID
	if v, err := h.Handle(ctx, req); err != nil || v.Absent {
		t.Fatalf("validate: %+v %v", v, err)
	}
	req.Hook = "outputs"
	out, err := h.Handle(ctx, req)
	if err != nil || out.Outputs["STRIPE_WEBHOOK_ENDPOINT_ID"] != "we_123" {
		t.Fatalf("outputs: %+v %v", out, err)
	}
	if _, leaked := out.Outputs["STRIPE_WEBHOOK_SECRET"]; leaked {
		t.Fatal("outputs hook must not claim the creation-only secret")
	}
	req.Hook = "destroy"
	if gone, err := h.Handle(ctx, req); err != nil || !gone.Absent {
		t.Fatalf("destroy: %+v %v", gone, err)
	}
	req.Hook = "read"
	if again, err := h.Handle(ctx, req); err != nil || !again.Absent {
		t.Fatalf("read after destroy: %+v %v", again, err)
	}
}

func TestReadFindsEndpointByName(t *testing.T) {
	t.Setenv("STRIPE_SECRET_KEY", "rk_test_fixture")
	useTransport(t, func(r *http.Request) (*http.Response, error) {
		return respond(200, `{"data":[{"id":"we_mine","metadata":{"envtie_name":"pr-12-stripe_webhook"}}],"has_more":false}`), nil
	})
	got, err := Handler{}.Handle(context.Background(), request("read"))
	if err != nil || got.ExternalID != "we_mine" {
		t.Fatalf("read: %+v %v", got, err)
	}
}

func TestCreateRetriesWithSameIdempotencyKey(t *testing.T) {
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_fixture")
	old := retryDelay
	retryDelay = 0
	t.Cleanup(func() { retryDelay = old })
	calls := 0
	useTransport(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Idempotency-Key") != "plan-1/stripe_webhook" {
			t.Fatal("retry changed the idempotency key")
		}
		if calls == 1 {
			return nil, errors.New("connection reset")
		}
		return respond(200, `{"id":"we_123","secret":"whsec_replayed"}`), nil
	})
	got, err := Handler{}.Handle(context.Background(), request("apply"))
	if err != nil || calls != 2 || got.Outputs["STRIPE_WEBHOOK_SECRET"] != "whsec_replayed" {
		t.Fatalf("apply: %+v %v calls=%d", got, err, calls)
	}
}

func TestErrorCodes(t *testing.T) {
	cases := []struct {
		name, key, body string
		status          int
		config          map[string]string
		want            string
	}{
		{name: "live key refused", key: "sk_live_x", want: "stripe_live_key_refused"},
		{name: "unknown key", key: "pk_test_x", want: "stripe_key_invalid"},
		{name: "missing key", key: "", want: "stripe_key_missing"},
		{name: "bad host", key: "sk_test_x", config: map[string]string{"host": "localhost"}, want: "stripe_host_invalid"},
		{name: "bad event", key: "sk_test_x", config: map[string]string{"events": "not an event"}, want: "stripe_event_invalid"},
		{name: "no events", key: "sk_test_x", config: map[string]string{"events": " , "}, want: "stripe_events_required"},
		{name: "endpoint limit", key: "sk_test_x", status: 400, body: `{"error":{"message":"You have reached the maximum of 16 test webhook endpoints."}}`, want: "stripe_endpoint_limit"},
		{name: "unauthorized", key: "sk_test_x", status: 401, body: `{}`, want: "stripe_unauthorized"},
		{name: "restricted key lacks permission", key: "rk_test_x", status: 403, body: `{}`, want: "stripe_permission_denied"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("STRIPE_SECRET_KEY", tc.key)
			useTransport(t, func(r *http.Request) (*http.Response, error) {
				if tc.status == 0 {
					t.Fatal("request must not be sent")
				}
				return respond(tc.status, tc.body), nil
			})
			req := request("apply")
			for k, v := range tc.config {
				req.Config[k] = v
			}
			_, err := Handler{}.Handle(context.Background(), req)
			var coded *adapter.CodedError
			if !errors.As(err, &coded) || coded.Code != tc.want {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
}

func TestLiveKeyAllowedWithExplicitOptIn(t *testing.T) {
	t.Setenv("STRIPE_SECRET_KEY", "sk_live_x")
	useTransport(t, func(r *http.Request) (*http.Response, error) {
		return respond(200, `{"id":"we_live","secret":"whsec_live"}`), nil
	})
	req := request("apply")
	req.Config["allow_live"] = "true"
	if got, err := (Handler{}).Handle(context.Background(), req); err != nil || got.ExternalID != "we_live" {
		t.Fatalf("apply: %+v %v", got, err)
	}
}
