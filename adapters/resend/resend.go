// Package resend creates one domain-scoped Resend sending API key per
// environment and delivers it for the app under test.
package resend

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/coddicat/envtie-core/adapters/httpclient"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

var (
	apiBase     = "https://api.resend.com"
	domainIDPat = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	fromAddrPat = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	maxKeyName  = 50
)

// Handler implements the EnvTie adapter protocol for Resend API keys.
type Handler struct{}

type apiKey struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type createResponse struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

type listResponse struct {
	Data []apiKey `json:"data"`
}

func (Handler) Manifest() adapter.Manifest {
	return adapter.Manifest{
		Name: "resend", Version: "1.0.0", Protocol: 1,
		Capabilities:  []string{"read", "apply", "validate", "outputs", "destroy"},
		CredentialEnv: []string{"RESEND_API_KEY"},
		Inputs: []adapter.Input{
			{Name: "RESEND_API_KEY", Kind: adapter.InputSecret, Required: true, Description: "Resend API key with permission to create and delete API keys (typically full_access)"},
		},
		Outputs: []adapter.Field{
			{Name: "RESEND_API_KEY_ID", Sensitivity: "PUBLIC"},
			{Name: "RESEND_API_KEY", Sensitivity: "SECRET", Availability: adapter.AvailabilityCreate},
			{Name: "RESEND_FROM", Sensitivity: "PUBLIC"},
		},
	}
}

func (Handler) Handle(ctx context.Context, r adapter.Request) (adapter.Response, error) {
	token := strings.TrimSpace(os.Getenv("RESEND_API_KEY"))
	if token == "" {
		return adapter.Response{}, adapter.NewError("resend_credential_missing")
	}
	auth := "Bearer " + token
	domainID := strings.TrimSpace(r.Config["domain_id"])
	if !domainIDPat.MatchString(domainID) {
		return adapter.Response{}, adapter.NewError("resend_domain_required")
	}
	from := strings.TrimSpace(r.Config["from"])
	if from != "" && !fromAddrPat.MatchString(from) {
		return adapter.Response{}, adapter.NewError("resend_from_invalid")
	}
	name := keyName(strings.TrimSpace(r.Config["name"]))
	switch r.Hook {
	case "read", "validate":
		if r.ExternalID == "" {
			if name == "" {
				return adapter.Response{Absent: true}, nil
			}
			found, err := findByName(ctx, auth, name)
			if err != nil {
				return adapter.Response{}, err
			}
			if found == nil {
				return adapter.Response{Absent: true}, nil
			}
			return adapter.Response{ExternalID: found.ID, Status: "ready"}, nil
		}
		found, err := findByID(ctx, auth, r.ExternalID)
		if err != nil {
			return adapter.Response{}, err
		}
		if found == nil {
			return adapter.Response{Absent: true}, nil
		}
		if name != "" && found.Name != name {
			return adapter.Response{}, adapter.NewError("resend_key_unmanaged")
		}
		return adapter.Response{ExternalID: found.ID, Status: "ready"}, nil
	case "apply":
		if name == "" {
			return adapter.Response{}, adapter.NewError("resend_name_required")
		}
		if existing, err := findByName(ctx, auth, name); err != nil {
			return adapter.Response{}, err
		} else if existing != nil {
			return adapter.Response{
				ExternalID: existing.ID,
				Status:     "ready",
				Outputs:    publicOutputs(existing.ID, from),
			}, nil
		}
		created, status, err := createKey(ctx, auth, name, domainID)
		if err != nil {
			return adapter.Response{}, classify(status)
		}
		out := publicOutputs(created.ID, from)
		out["RESEND_API_KEY"] = created.Token
		return adapter.Response{
			ExternalID: created.ID,
			Status:     "created",
			Outputs:    out,
		}, nil
	case "outputs":
		if r.ExternalID == "" {
			return adapter.Response{}, adapter.NewError("resend_key_missing")
		}
		found, err := findByID(ctx, auth, r.ExternalID)
		if err != nil {
			return adapter.Response{}, err
		}
		if found == nil {
			return adapter.Response{}, adapter.NewError("resend_key_missing")
		}
		if name != "" && found.Name != name {
			return adapter.Response{}, adapter.NewError("resend_key_unmanaged")
		}
		return adapter.Response{Outputs: publicOutputs(found.ID, from)}, nil
	case "destroy":
		id := r.ExternalID
		if id == "" && name != "" {
			found, err := findByName(ctx, auth, name)
			if err != nil {
				return adapter.Response{}, err
			}
			if found == nil {
				return adapter.Response{Absent: true}, nil
			}
			id = found.ID
		}
		if id == "" {
			return adapter.Response{Absent: true}, nil
		}
		status, err := httpclient.JSON(ctx, http.MethodDelete, apiBase+"/api-keys/"+url.PathEscape(id), auth, nil, nil)
		if status == http.StatusNotFound {
			return adapter.Response{Absent: true}, nil
		}
		if err != nil {
			return adapter.Response{}, classify(status)
		}
		return adapter.Response{Absent: true}, nil
	default:
		return adapter.Response{}, adapter.NewError("resend_invalid_request")
	}
}

func publicOutputs(id, from string) map[string]string {
	out := map[string]string{"RESEND_API_KEY_ID": id}
	if from != "" {
		out["RESEND_FROM"] = from
	}
	return out
}

func keyName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if len(name) > maxKeyName {
		return name[:maxKeyName]
	}
	return name
}

func findByName(ctx context.Context, auth, name string) (*apiKey, error) {
	list, status, err := listKeys(ctx, auth)
	if err != nil {
		return nil, classify(status)
	}
	for i := range list {
		if list[i].Name == name {
			return &list[i], nil
		}
	}
	return nil, nil
}

func findByID(ctx context.Context, auth, id string) (*apiKey, error) {
	list, status, err := listKeys(ctx, auth)
	if err != nil {
		return nil, classify(status)
	}
	for i := range list {
		if list[i].ID == id {
			return &list[i], nil
		}
	}
	return nil, nil
}

func listKeys(ctx context.Context, auth string) ([]apiKey, int, error) {
	var resp listResponse
	status, err := httpclient.JSON(ctx, http.MethodGet, apiBase+"/api-keys", auth, nil, &resp)
	if err != nil {
		return nil, status, err
	}
	return resp.Data, status, nil
}

func createKey(ctx context.Context, auth, name, domainID string) (*createResponse, int, error) {
	body := map[string]any{
		"name":       name,
		"permission": "sending_access",
		"domain_id":  domainID,
	}
	var created createResponse
	status, err := httpclient.JSON(ctx, http.MethodPost, apiBase+"/api-keys", auth, body, &created)
	if err != nil {
		return nil, status, err
	}
	if created.ID == "" || created.Token == "" {
		return nil, status, adapter.NewError("resend_unavailable")
	}
	return &created, status, nil
}

func classify(status int) error {
	switch httpclient.StatusClass(status) {
	case "auth":
		return adapter.NewError("resend_unauthorized")
	case "not_found":
		return adapter.NewError("resend_not_found")
	case "rate_limit":
		return adapter.NewError("resend_rate_limited")
	case "invalid":
		return adapter.NewError("resend_invalid_request")
	case "quota":
		return adapter.NewError("resend_quota_exceeded")
	default:
		return adapter.NewError("resend_unavailable")
	}
}
