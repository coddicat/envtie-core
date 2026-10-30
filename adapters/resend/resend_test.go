package resend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coddicat/envtie-core/adapters/httpclient"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

func TestLifecycleCreatesSendingKey(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "re_manage")
	created := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer re_manage" {
			t.Errorf("auth %s", r.Header.Get("Authorization"))
		}
		if r.Header.Get("User-Agent") == "" {
			t.Error("missing User-Agent")
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api-keys":
			if !created {
				_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data":   []map[string]string{{"id": "key_1", "name": "pr-7-resend"}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api-keys":
			body, _ := io.ReadAll(r.Body)
			var in map[string]any
			_ = json.Unmarshal(body, &in)
			if in["name"] != "pr-7-resend" || in["permission"] != "sending_access" || in["domain_id"] != "d91cd9bd-1176-453e-8fc1-35364d380206" {
				t.Errorf("create body %s", body)
			}
			created = true
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id": "key_1", "object": "api_key", "token": "re_preview_secret",
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/api-keys/key_1":
			created = false
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "api_key", "id": "key_1", "deleted": true})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	oldBase, oldClient := apiBase, httpclient.Client
	t.Cleanup(func() {
		apiBase = oldBase
		httpclient.Client = oldClient
	})
	apiBase = srv.URL
	httpclient.Client = srv.Client()

	cfg := map[string]string{
		"domain_id": "d91cd9bd-1176-453e-8fc1-35364d380206",
		"from":      "preview@example.com",
		"name":      "pr-7-resend",
	}
	h := Handler{}
	resp, err := h.Handle(context.Background(), adapter.Request{Hook: "apply", Environment: "pr-7", Component: "resend", Config: cfg})
	if err != nil || resp.ExternalID != "key_1" || resp.Outputs["RESEND_API_KEY"] != "re_preview_secret" || resp.Outputs["RESEND_FROM"] != "preview@example.com" {
		t.Fatalf("apply %+v %v", resp, err)
	}
	resp, err = h.Handle(context.Background(), adapter.Request{Hook: "read", Environment: "pr-7", Component: "resend", Config: cfg, ExternalID: "key_1"})
	if err != nil || resp.Absent || resp.ExternalID != "key_1" {
		t.Fatalf("read %+v %v", resp, err)
	}
	resp, err = h.Handle(context.Background(), adapter.Request{Hook: "outputs", Environment: "pr-7", Component: "resend", Config: cfg, ExternalID: "key_1"})
	if err != nil || resp.Outputs["RESEND_API_KEY"] != "" || resp.Outputs["RESEND_API_KEY_ID"] != "key_1" {
		t.Fatalf("outputs %+v %v", resp, err)
	}
	resp, err = h.Handle(context.Background(), adapter.Request{Hook: "destroy", Environment: "pr-7", Component: "resend", Config: cfg, ExternalID: "key_1"})
	if err != nil || !resp.Absent {
		t.Fatalf("destroy %+v %v", resp, err)
	}
}

func TestMissingCredential(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "")
	_, err := Handler{}.Handle(context.Background(), adapter.Request{
		Hook: "apply", Config: map[string]string{"domain_id": "d91cd9bd-1176-453e-8fc1-35364d380206", "name": "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "resend_credential_missing") {
		t.Fatalf("%v", err)
	}
}

func TestDomainRequired(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "re_x")
	_, err := Handler{}.Handle(context.Background(), adapter.Request{
		Hook: "apply", Config: map[string]string{"domain_id": "not-a-uuid", "name": "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "resend_domain_required") {
		t.Fatalf("%v", err)
	}
}

func TestManifest(t *testing.T) {
	m := Handler{}.Manifest()
	if m.Name != "resend" || m.CredentialEnv[0] != "RESEND_API_KEY" {
		t.Fatalf("%+v", m)
	}
}
