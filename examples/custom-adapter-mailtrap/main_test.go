package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/coddicat/envtie-core/sdk/adapter"
)

type roundTrip func(*http.Request) (int, string)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	status, body := f(r)
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    r,
	}, nil
}

func withAPI(t *testing.T, fn roundTrip) {
	t.Helper()
	oldClient, oldBase := client, apiBase
	t.Cleanup(func() {
		client = oldClient
		apiBase = oldBase
	})
	apiBase = "https://mailtrap.test/api"
	client = &http.Client{Transport: fn}
}

func TestLifecycleCreatesInboxAndReturnsSMTP(t *testing.T) {
	t.Setenv("MAILTRAP_API_TOKEN", "mt_fixture")
	created := false
	withAPI(t, func(r *http.Request) (int, string) {
		if r.Header.Get("Api-Token") != "mt_fixture" {
			t.Errorf("auth %s", r.Header.Get("Api-Token"))
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/projects/99/inboxes"):
			if !created {
				return http.StatusOK, `[]`
			}
			return http.StatusOK, `[{"id":42,"name":"pr-7-mail","username":"user","password":"pass","smtp_ports":[2525],"project_id":99,"status":"active"}]`
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/projects/99/inboxes"):
			body, _ := io.ReadAll(r.Body)
			var in map[string]any
			_ = json.Unmarshal(body, &in)
			inbox, _ := in["inbox"].(map[string]any)
			if inbox["name"] != "pr-7-mail" {
				t.Errorf("create body %s", body)
			}
			created = true
			return http.StatusOK, `{"id":42,"name":"pr-7-mail","username":"user","password":"secret-pass","smtp_ports":[2525,587],"project_id":99,"status":"active"}`
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/inboxes/42"):
			return http.StatusOK, `{"id":42,"name":"pr-7-mail","username":"user","password":"secret-pass","smtp_ports":[2525],"project_id":99,"status":"active"}`
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/inboxes/42"):
			created = false
			return http.StatusOK, `{}`
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return http.StatusNotFound, `{}`
		}
	})
	cfg := map[string]string{"account_id": "1", "project_id": "99", "name": "pr-7-mail"}
	h := handler{}
	resp, err := h.Handle(context.Background(), adapter.Request{Hook: "apply", Environment: "pr-7", Component: "mail", Config: cfg})
	if err != nil || resp.ExternalID != "42" || resp.Outputs["MAILTRAP_SMTP_PASS"] != "secret-pass" || resp.Outputs["MAILTRAP_SMTP_HOST"] != "sandbox.smtp.mailtrap.io" {
		t.Fatalf("apply %+v %v", resp, err)
	}
	resp, err = h.Handle(context.Background(), adapter.Request{Hook: "read", Environment: "pr-7", Component: "mail", Config: cfg, ExternalID: "42"})
	if err != nil || resp.Absent || resp.ExternalID != "42" {
		t.Fatalf("read %+v %v", resp, err)
	}
	resp, err = h.Handle(context.Background(), adapter.Request{Hook: "destroy", Environment: "pr-7", Component: "mail", Config: cfg, ExternalID: "42"})
	if err != nil || !resp.Absent {
		t.Fatalf("destroy %+v %v", resp, err)
	}
}

func TestMissingTokenFailsBeforeProviderCall(t *testing.T) {
	t.Setenv("MAILTRAP_API_TOKEN", "")
	called := false
	withAPI(t, func(r *http.Request) (int, string) {
		called = true
		return http.StatusOK, `{}`
	})
	_, err := handler{}.Handle(context.Background(), adapter.Request{
		Hook: "apply", Environment: "pr-7", Component: "mail",
		Config: map[string]string{"account_id": "1", "project_id": "99", "name": "pr-7-mail"},
	})
	if err == nil || !strings.Contains(err.Error(), "mailtrap_credential_missing") || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}
