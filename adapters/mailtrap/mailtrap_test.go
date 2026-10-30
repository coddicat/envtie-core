package mailtrap

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
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/1/inboxes"):
			if !created {
				return http.StatusOK, `[]`
			}
			return http.StatusOK, `[{"id":42,"name":"pr-7-mail","username":"user","password":"pass","smtp_ports":[2525],"project_id":99,"status":"active"}]`
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/projects/99/sandboxes"):
			body, _ := io.ReadAll(r.Body)
			var in map[string]any
			_ = json.Unmarshal(body, &in)
			sandbox, _ := in["sandbox"].(map[string]any)
			if sandbox["name"] != "pr-7-mail" {
				t.Errorf("create body %s", body)
			}
			created = true
			return http.StatusOK, `{"id":42,"name":"pr-7-mail","username":"user","password":"secret-pass","smtp_ports":[2525,587],"project_id":99,"status":"active"}`
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sandboxes/42"):
			return http.StatusOK, `{"id":42,"name":"pr-7-mail","username":"user","password":"secret-pass","smtp_ports":[2525],"project_id":99,"status":"active"}`
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/sandboxes/42"):
			created = false
			return http.StatusOK, `{}`
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return http.StatusNotFound, `{}`
		}
	})
	cfg := map[string]string{"account_id": "1", "project_id": "99", "name": "pr-7-mail"}
	h := Handler{}
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
	_, err := Handler{}.Handle(context.Background(), adapter.Request{
		Hook: "apply", Environment: "pr-7", Component: "mail",
		Config: map[string]string{"account_id": "1", "project_id": "99", "name": "pr-7-mail"},
	})
	if err == nil || !strings.Contains(err.Error(), "mailtrap_credential_missing") || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestManifestDeclaresCredential(t *testing.T) {
	m := Handler{}.Manifest()
	if m.Name != "mailtrap" || len(m.CredentialEnv) != 1 || m.CredentialEnv[0] != "MAILTRAP_API_TOKEN" {
		t.Fatalf("%+v", m)
	}
}

func TestCreateForbiddenMapsToQuota(t *testing.T) {
	t.Setenv("MAILTRAP_API_TOKEN", "mt_fixture")
	withAPI(t, func(r *http.Request) (int, string) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/1/inboxes"):
			return http.StatusOK, `[]`
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/projects/99/sandboxes"):
			return http.StatusForbidden, `{"errors":"Access forbidden"}`
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return http.StatusNotFound, `{}`
		}
	})
	_, err := Handler{}.Handle(context.Background(), adapter.Request{
		Hook: "apply", Environment: "pr-7", Component: "mail",
		Config: map[string]string{"account_id": "1", "project_id": "99", "name": "pr-7-mail"},
	})
	if err == nil || !strings.Contains(err.Error(), "mailtrap_quota_exceeded") {
		t.Fatalf("err=%v", err)
	}
}

func TestSharedModeCreatesOnceAndReuses(t *testing.T) {
	t.Setenv("MAILTRAP_API_TOKEN", "mt_fixture")
	creates := 0
	withAPI(t, func(r *http.Request) (int, string) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/1/inboxes"):
			if creates == 0 {
				return http.StatusOK, `[]`
			}
			return http.StatusOK, `[{"id":7,"name":"envtie-shared","username":"shared-user","password":"shared-pass","smtp_ports":[2525],"project_id":99,"status":"active"}]`
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/projects/99/sandboxes"):
			body, _ := io.ReadAll(r.Body)
			var in map[string]any
			_ = json.Unmarshal(body, &in)
			sandbox, _ := in["sandbox"].(map[string]any)
			if sandbox["name"] != "envtie-shared" {
				t.Errorf("create body %s", body)
			}
			creates++
			return http.StatusOK, `{"id":7,"name":"envtie-shared","username":"shared-user","password":"shared-pass","smtp_ports":[2525],"project_id":99,"status":"active"}`
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return http.StatusNotFound, `{}`
		}
	})
	cfg := map[string]string{
		"account_id": "1", "project_id": "99",
		"mode": "shared", "recipient_domain": "mail.example.test",
		"name": "pr-7-mail", // engine-injected; shared mode must ignore for identity
	}
	h := Handler{}
	first, err := h.Handle(context.Background(), adapter.Request{Hook: "apply", Environment: "pr-7", Component: "mail", Config: cfg})
	if err != nil || first.ExternalID != "7" || first.Status != "created" || first.Outputs["MAILTRAP_SMTP_PASS"] != "shared-pass" {
		t.Fatalf("first apply %+v %v", first, err)
	}
	if creates != 1 {
		t.Fatalf("creates=%d", creates)
	}
	second, err := h.Handle(context.Background(), adapter.Request{Hook: "apply", Environment: "pr-8", Component: "mail", Config: cfg})
	if err != nil || second.ExternalID != "7" || second.Status != "ready" || second.Outputs["MAILTRAP_SMTP_PASS"] != "" {
		t.Fatalf("second apply %+v %v", second, err)
	}
	if creates != 1 {
		t.Fatalf("second apply created again: creates=%d", creates)
	}
}

func TestSharedModeDestroyDoesNotDelete(t *testing.T) {
	t.Setenv("MAILTRAP_API_TOKEN", "mt_fixture")
	deleted := false
	withAPI(t, func(r *http.Request) (int, string) {
		if r.Method == http.MethodDelete {
			deleted = true
			t.Errorf("shared destroy must not call %s %s", r.Method, r.URL.Path)
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		return http.StatusOK, `{}`
	})
	resp, err := Handler{}.Handle(context.Background(), adapter.Request{
		Hook: "destroy", Environment: "pr-7", Component: "mail", ExternalID: "7",
		Config: map[string]string{
			"account_id": "1", "project_id": "99",
			"mode": "shared", "recipient_domain": "mail.example.test",
		},
	})
	if err != nil || !resp.Absent || deleted {
		t.Fatalf("destroy %+v err=%v deleted=%v", resp, err, deleted)
	}
}

func TestSharedModeRecipientTagStableAndUnique(t *testing.T) {
	t.Setenv("MAILTRAP_API_TOKEN", "mt_fixture")
	withAPI(t, func(r *http.Request) (int, string) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/1/inboxes"):
			return http.StatusOK, `[{"id":7,"name":"qa-inbox","username":"u","password":"p","smtp_ports":[2525],"project_id":99,"status":"active"}]`
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sandboxes/7"):
			return http.StatusOK, `{"id":7,"name":"qa-inbox","username":"u","password":"p","smtp_ports":[2525],"project_id":99,"status":"active"}`
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return http.StatusNotFound, `{}`
		}
	})
	cfg := map[string]string{
		"account_id": "1", "project_id": "99",
		"mode": "shared", "shared_sandbox_name": "qa-inbox",
		"recipient_domain": "mail.example.test",
	}
	h := Handler{}
	a1, err := h.Handle(context.Background(), adapter.Request{Hook: "apply", Environment: "pr-7", Component: "mail", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := h.Handle(context.Background(), adapter.Request{Hook: "apply", Environment: "pr-7", Component: "mail", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Handle(context.Background(), adapter.Request{Hook: "apply", Environment: "pr-8", Component: "mail", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if a1.Outputs[recipientTagOutput] != "pr-7@mail.example.test" {
		t.Fatalf("tag=%q", a1.Outputs[recipientTagOutput])
	}
	if a1.Outputs[recipientTagOutput] != a2.Outputs[recipientTagOutput] {
		t.Fatalf("tag not stable: %q vs %q", a1.Outputs[recipientTagOutput], a2.Outputs[recipientTagOutput])
	}
	if b.Outputs[recipientTagOutput] != "pr-8@mail.example.test" {
		t.Fatalf("other env tag=%q", b.Outputs[recipientTagOutput])
	}
	if a1.Outputs[recipientTagOutput] == b.Outputs[recipientTagOutput] {
		t.Fatal("tags must differ per environment")
	}
	out, err := h.Handle(context.Background(), adapter.Request{
		Hook: "outputs", Environment: "pr-7", Component: "mail", ExternalID: "7", Config: cfg,
	})
	if err != nil || out.Outputs[recipientTagOutput] != "pr-7@mail.example.test" {
		t.Fatalf("outputs %+v %v", out, err)
	}
}

func TestInvalidModeFailsValidation(t *testing.T) {
	t.Setenv("MAILTRAP_API_TOKEN", "mt_fixture")
	called := false
	withAPI(t, func(r *http.Request) (int, string) {
		called = true
		return http.StatusOK, `{}`
	})
	_, err := Handler{}.Handle(context.Background(), adapter.Request{
		Hook: "apply", Environment: "pr-7", Component: "mail",
		Config: map[string]string{"account_id": "1", "project_id": "99", "mode": "pooled", "name": "pr-7-mail"},
	})
	if err == nil || !strings.Contains(err.Error(), "mailtrap_mode_invalid") || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestSharedModeRequiresRecipientDomain(t *testing.T) {
	t.Setenv("MAILTRAP_API_TOKEN", "mt_fixture")
	called := false
	withAPI(t, func(r *http.Request) (int, string) {
		called = true
		return http.StatusOK, `{}`
	})
	_, err := Handler{}.Handle(context.Background(), adapter.Request{
		Hook: "apply", Environment: "pr-7", Component: "mail",
		Config: map[string]string{"account_id": "1", "project_id": "99", "mode": "shared"},
	})
	if err == nil || !strings.Contains(err.Error(), "mailtrap_recipient_domain_required") || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestSharedModeRejectsUnmanagedSandboxName(t *testing.T) {
	t.Setenv("MAILTRAP_API_TOKEN", "mt_fixture")
	withAPI(t, func(r *http.Request) (int, string) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sandboxes/7") {
			return http.StatusOK, `{"id":7,"name":"someone-elses-inbox","username":"u","password":"p","smtp_ports":[2525],"project_id":99,"status":"active"}`
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		return http.StatusNotFound, `{}`
	})
	_, err := Handler{}.Handle(context.Background(), adapter.Request{
		Hook: "validate", Environment: "pr-7", Component: "mail", ExternalID: "7",
		Config: map[string]string{
			"account_id": "1", "project_id": "99",
			"mode": "shared", "recipient_domain": "mail.example.test",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "mailtrap_inbox_unmanaged") {
		t.Fatalf("err=%v", err)
	}
}

func TestSharedModeReadReportsAbsentLikeReferenced(t *testing.T) {
	t.Setenv("MAILTRAP_API_TOKEN", "mt_fixture")
	called := false
	withAPI(t, func(r *http.Request) (int, string) {
		called = true
		return http.StatusOK, `{}`
	})
	resp, err := Handler{}.Handle(context.Background(), adapter.Request{
		Hook: "read", Environment: "pr-7", Component: "mail", ExternalID: "7",
		Config: map[string]string{
			"account_id": "1", "project_id": "99",
			"mode": "shared", "recipient_domain": "mail.example.test",
		},
	})
	if err != nil || !resp.Absent || called {
		t.Fatalf("read %+v err=%v called=%v", resp, err, called)
	}
}
