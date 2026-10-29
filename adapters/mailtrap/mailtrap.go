// Package mailtrap creates one Mailtrap Email Sandbox inbox per environment
// and delivers SMTP credentials for the app under test.
package mailtrap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/coddicat/envtie-core/sdk/adapter"
)

var (
	apiBase = "https://mailtrap.io/api"
	client  = &http.Client{Timeout: 30 * time.Second}
	idNum   = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
)

// Handler implements the EnvTie adapter protocol for Mailtrap Email Sandboxes.
type Handler struct{}

type inbox struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	Status    string `json:"status"`
	ProjectID int64  `json:"project_id"`
	SMTPPorts []int  `json:"smtp_ports"`
}

func (Handler) Manifest() adapter.Manifest {
	return adapter.Manifest{
		Name: "mailtrap", Version: "1.0.0", Protocol: 1,
		Capabilities:  []string{"read", "apply", "validate", "outputs", "destroy"},
		CredentialEnv: []string{"MAILTRAP_API_TOKEN"},
		Inputs: []adapter.Input{
			{Name: "MAILTRAP_API_TOKEN", Kind: adapter.InputSecret, Required: true, Description: "Mailtrap API token with Email Sandbox access"},
		},
		Outputs: []adapter.Field{
			{Name: "MAILTRAP_INBOX_ID", Sensitivity: "PUBLIC"},
			{Name: "MAILTRAP_SMTP_HOST", Sensitivity: "PUBLIC"},
			{Name: "MAILTRAP_SMTP_PORT", Sensitivity: "PUBLIC"},
			{Name: "MAILTRAP_SMTP_USER", Sensitivity: "SECRET"},
			{Name: "MAILTRAP_SMTP_PASS", Sensitivity: "SECRET", Availability: adapter.AvailabilityCreate},
		},
	}
}

func (Handler) Handle(ctx context.Context, r adapter.Request) (adapter.Response, error) {
	token := strings.TrimSpace(os.Getenv("MAILTRAP_API_TOKEN"))
	if token == "" {
		return adapter.Response{}, adapter.NewError("mailtrap_credential_missing")
	}
	accountID := strings.TrimSpace(r.Config["account_id"])
	projectID := strings.TrimSpace(r.Config["project_id"])
	if !idNum.MatchString(accountID) {
		return adapter.Response{}, adapter.NewError("mailtrap_account_required")
	}
	if !idNum.MatchString(projectID) {
		return adapter.Response{}, adapter.NewError("mailtrap_project_required")
	}
	name := strings.TrimSpace(r.Config["name"])
	switch r.Hook {
	case "read", "validate":
		if r.ExternalID == "" {
			if name == "" {
				return adapter.Response{Absent: true}, nil
			}
			found, err := findByName(ctx, token, accountID, projectID, name)
			if err != nil {
				return adapter.Response{}, err
			}
			if found == nil {
				return adapter.Response{Absent: true}, nil
			}
			return adapter.Response{ExternalID: strconv.FormatInt(found.ID, 10), Status: "ready"}, nil
		}
		box, status, err := getInbox(ctx, token, accountID, r.ExternalID)
		if status == http.StatusNotFound {
			return adapter.Response{Absent: true}, nil
		}
		if err != nil {
			return adapter.Response{}, classify(status, err)
		}
		if name != "" && box.Name != name {
			return adapter.Response{}, adapter.NewError("mailtrap_inbox_unmanaged")
		}
		return adapter.Response{ExternalID: strconv.FormatInt(box.ID, 10), Status: "ready"}, nil
	case "apply":
		if name == "" {
			return adapter.Response{}, adapter.NewError("mailtrap_name_required")
		}
		if existing, err := findByName(ctx, token, accountID, projectID, name); err != nil {
			return adapter.Response{}, err
		} else if existing != nil {
			return adapter.Response{
				ExternalID: strconv.FormatInt(existing.ID, 10),
				Status:     "ready",
				Outputs:    publicOutputs(existing),
			}, nil
		}
		box, status, err := createInbox(ctx, token, accountID, projectID, name)
		if err != nil {
			return adapter.Response{}, classify(status, err)
		}
		out := publicOutputs(box)
		out["MAILTRAP_SMTP_PASS"] = box.Password
		return adapter.Response{
			ExternalID: strconv.FormatInt(box.ID, 10),
			Status:     "created",
			Outputs:    out,
		}, nil
	case "outputs":
		if r.ExternalID == "" {
			return adapter.Response{}, adapter.NewError("mailtrap_inbox_missing")
		}
		box, status, err := getInbox(ctx, token, accountID, r.ExternalID)
		if err != nil {
			return adapter.Response{}, classify(status, err)
		}
		if name != "" && box.Name != name {
			return adapter.Response{}, adapter.NewError("mailtrap_inbox_unmanaged")
		}
		return adapter.Response{Outputs: publicOutputs(box)}, nil
	case "destroy":
		id := r.ExternalID
		if id == "" && name != "" {
			found, err := findByName(ctx, token, accountID, projectID, name)
			if err != nil {
				return adapter.Response{}, err
			}
			if found == nil {
				return adapter.Response{Absent: true}, nil
			}
			id = strconv.FormatInt(found.ID, 10)
		}
		if id == "" {
			return adapter.Response{Absent: true}, nil
		}
		status, err := deleteInbox(ctx, token, accountID, id)
		if status == http.StatusNotFound {
			return adapter.Response{Absent: true}, nil
		}
		if err != nil {
			return adapter.Response{}, classify(status, err)
		}
		return adapter.Response{Absent: true}, nil
	default:
		return adapter.Response{}, adapter.NewError("mailtrap_invalid_request")
	}
}

func publicOutputs(box *inbox) map[string]string {
	port := "2525"
	for _, p := range box.SMTPPorts {
		if p == 2525 || p == 587 || p == 25 || p == 465 {
			port = strconv.Itoa(p)
			break
		}
	}
	if len(box.SMTPPorts) > 0 && port == "2525" {
		port = strconv.Itoa(box.SMTPPorts[0])
	}
	return map[string]string{
		"MAILTRAP_INBOX_ID":  strconv.FormatInt(box.ID, 10),
		"MAILTRAP_SMTP_HOST": "sandbox.smtp.mailtrap.io",
		"MAILTRAP_SMTP_PORT": port,
		"MAILTRAP_SMTP_USER": box.Username,
	}
}

func findByName(ctx context.Context, token, accountID, projectID, name string) (*inbox, error) {
	list, status, err := listInboxes(ctx, token, accountID)
	if err != nil {
		return nil, classify(status, err)
	}
	wantProject, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil {
		return nil, adapter.NewError("mailtrap_project_required")
	}
	for i := range list {
		if list[i].Name == name && list[i].ProjectID == wantProject {
			return &list[i], nil
		}
	}
	return nil, nil
}

// listInboxes uses the account-scoped list. Mailtrap documents
// GET /api/accounts/{account_id}/inboxes; GET .../projects/{id}/inboxes is
// create-only and returns 404 for list (mailtrap_not_found on read-before-create).
func listInboxes(ctx context.Context, token, accountID string) ([]inbox, int, error) {
	var list []inbox
	status, err := doJSON(ctx, http.MethodGet, apiBase+"/accounts/"+url.PathEscape(accountID)+"/inboxes", token, nil, &list)
	return list, status, err
}

func getInbox(ctx context.Context, token, accountID, inboxID string) (*inbox, int, error) {
	var box inbox
	status, err := doJSON(ctx, http.MethodGet, apiBase+"/accounts/"+url.PathEscape(accountID)+"/inboxes/"+url.PathEscape(inboxID), token, nil, &box)
	if err != nil {
		return nil, status, err
	}
	return &box, status, nil
}

func createInbox(ctx context.Context, token, accountID, projectID, name string) (*inbox, int, error) {
	var box inbox
	// Mailtrap create accepts {"name":"..."} on the project inboxes path.
	body := map[string]any{"name": name}
	status, err := doJSON(ctx, http.MethodPost, apiBase+"/accounts/"+url.PathEscape(accountID)+"/projects/"+url.PathEscape(projectID)+"/inboxes", token, body, &box)
	if err != nil {
		return nil, status, err
	}
	return &box, status, nil
}

func deleteInbox(ctx context.Context, token, accountID, inboxID string) (int, error) {
	return doJSON(ctx, http.MethodDelete, apiBase+"/accounts/"+url.PathEscape(accountID)+"/inboxes/"+url.PathEscape(inboxID), token, nil, nil)
}

func doJSON(ctx context.Context, method, endpoint, token string, body any, target any) (int, error) {
	var input io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, adapter.NewError("mailtrap_unavailable")
		}
		input = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, input)
	if err != nil {
		return 0, adapter.NewError("mailtrap_unavailable")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Api-Token", token)
	req.Header.Set("User-Agent", "EnvTie.dev-adapter-mailtrap/1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, adapter.NewError("mailtrap_unavailable")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("mailtrap http %d", resp.StatusCode)
	}
	if target != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, target); err != nil {
			return resp.StatusCode, adapter.NewError("mailtrap_unavailable")
		}
	}
	return resp.StatusCode, nil
}

func classify(status int, err error) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return adapter.NewError("mailtrap_unauthorized")
	case http.StatusNotFound:
		return adapter.NewError("mailtrap_not_found")
	case http.StatusTooManyRequests:
		return adapter.NewError("mailtrap_rate_limited")
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return adapter.NewError("mailtrap_invalid_request")
	default:
		if err != nil {
			return adapter.NewError("mailtrap_unavailable")
		}
		return adapter.NewError("mailtrap_unavailable")
	}
}
