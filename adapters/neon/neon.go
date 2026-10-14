package neon

import (
	"context"
	"net/url"
	"os"

	"github.com/coddicat/envtie-core/adapters/httpclient"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

type Handler struct{}

func (Handler) Manifest() adapter.Manifest {
	return adapter.Manifest{
		Name: "neon", Version: "1.0.0", Protocol: 1,
		Capabilities:  []string{"read", "apply", "validate", "outputs", "destroy"},
		CredentialEnv: []string{"NEON_API_KEY"},
		Inputs: []adapter.Input{
			{Name: "NEON_API_KEY", Kind: adapter.InputSecret, Required: true, Description: "Neon API key"},
		},
		Outputs: []adapter.Field{{Name: "DATABASE_URL", Sensitivity: "SECRET"}},
	}
}

func (Handler) Handle(ctx context.Context, r adapter.Request) (adapter.Response, error) {
	// Required credentials and config are rejected before any provider call.
	// The runner's read-before-create is the non-mutating preflight: apply is
	// not attempted when that read fails.
	if err := preflight(r); err != nil {
		return adapter.Response{}, err
	}
	project := r.Config["project_id"]
	key := os.Getenv("NEON_API_KEY")
	base := "https://console.neon.tech/api/v2/projects/" + url.PathEscape(project)
	auth := "Bearer " + key
	switch r.Hook {
	case "read":
		if r.ExternalID != "" {
			var x map[string]any
			status, err := httpclient.JSON(ctx, "GET", base+"/branches/"+url.PathEscape(r.ExternalID), auth, nil, &x)
			if status == 404 {
				return adapter.Response{Absent: true}, nil
			}
			if err != nil {
				return adapter.Response{}, classify(status, "read")
			}
			return adapter.Response{ExternalID: r.ExternalID, Status: "ready"}, nil
		}
		var x struct {
			Branches []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"branches"`
		}
		q := url.Values{}
		q.Set("search", r.Config["name"])
		status, err := httpclient.JSON(ctx, "GET", base+"/branches?"+q.Encode(), auth, nil, &x)
		if err != nil {
			return adapter.Response{}, classify(status, "read")
		}
		for _, b := range x.Branches {
			if b.Name == r.Config["name"] {
				return adapter.Response{ExternalID: b.ID, Status: "ready"}, nil
			}
		}
		return adapter.Response{Absent: true}, nil
	case "apply":
		body := map[string]any{"branch": map[string]any{"name": r.Config["name"]}, "endpoints": []map[string]string{{"type": "read_write"}}}
		var x struct {
			Branch struct {
				ID string `json:"id"`
			} `json:"branch"`
		}
		status, err := httpclient.JSON(ctx, "POST", base+"/branches", auth, body, &x)
		if err != nil {
			return adapter.Response{}, classify(status, "apply")
		}
		return adapter.Response{ExternalID: x.Branch.ID, Status: "created"}, nil
	case "validate":
		return (Handler{}).Handle(ctx, adapter.Request{Hook: "read", Config: r.Config, ExternalID: r.ExternalID})
	case "outputs":
		q := url.Values{}
		q.Set("branch_id", r.ExternalID)
		q.Set("database_name", r.Config["database_name"])
		q.Set("role_name", r.Config["role_name"])
		var x struct {
			URI string `json:"uri"`
		}
		status, err := httpclient.JSON(ctx, "GET", base+"/connection_uri?"+q.Encode(), auth, nil, &x)
		if err != nil {
			return adapter.Response{}, classify(status, "outputs")
		}
		if x.URI == "" {
			return adapter.Response{}, adapter.NewError("neon_outputs_unavailable")
		}
		return adapter.Response{Outputs: map[string]string{"DATABASE_URL": x.URI}}, nil
	case "destroy":
		status, err := httpclient.JSON(ctx, "DELETE", base+"/branches/"+url.PathEscape(r.ExternalID), auth, nil, nil)
		if status == 404 || status == 204 {
			return adapter.Response{Absent: true}, nil
		}
		if err != nil {
			return adapter.Response{}, classify(status, "destroy")
		}
		return adapter.Response{Absent: true}, nil
	}
	return adapter.Response{}, adapter.NewError("neon_invalid_request")
}

func preflight(r adapter.Request) error {
	switch r.Hook {
	case "read", "apply", "validate", "outputs", "destroy":
	default:
		return adapter.NewError("neon_invalid_request")
	}
	if os.Getenv("NEON_API_KEY") == "" {
		return adapter.NewError("neon_credential_missing")
	}
	if r.Config["project_id"] == "" {
		return adapter.NewError("neon_project_required")
	}
	switch r.Hook {
	case "apply":
		if r.Config["name"] == "" {
			return adapter.NewError("neon_name_required")
		}
	case "read", "validate":
		if r.ExternalID == "" && r.Config["name"] == "" {
			return adapter.NewError("neon_name_required")
		}
	case "outputs":
		if r.ExternalID == "" {
			return adapter.NewError("neon_outputs_unavailable")
		}
		if r.Config["database_name"] == "" || r.Config["role_name"] == "" {
			return adapter.NewError("neon_database_role")
		}
	case "destroy":
		if r.ExternalID == "" {
			return adapter.NewError("neon_invalid_request")
		}
	}
	return nil
}

func classify(status int, phase string) error {
	if status < 400 {
		return adapter.NewError("neon_unavailable")
	}
	switch httpclient.StatusClass(status) {
	case "auth":
		return adapter.NewError("neon_unauthorized")
	case "not_found":
		if phase == "outputs" {
			return adapter.NewError("neon_database_role")
		}
		return adapter.NewError("neon_project_not_found")
	case "quota":
		return adapter.NewError("neon_quota")
	case "rate_limit":
		return adapter.NewError("neon_rate_limited")
	case "invalid":
		if phase == "outputs" {
			return adapter.NewError("neon_connection_uri")
		}
		return adapter.NewError("neon_invalid_request")
	case "unavailable":
		return adapter.NewError("neon_unavailable")
	default:
		return adapter.NewError("neon_invalid_request")
	}
}
