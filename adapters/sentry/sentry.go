// Package sentry creates one Sentry project per environment and delivers its DSN.
package sentry

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"unicode"

	"github.com/coddicat/envtie-core/adapters/httpclient"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

var apiBase = "https://sentry.io/api/0"

var (
	orgSlugPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,49}$`)
	teamSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,49}$`)
	platformPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
)

const defaultPlatform = "node-express"
const maxSlugLen = 50

type Handler struct{}

func (Handler) Manifest() adapter.Manifest {
	return adapter.Manifest{
		Name: "sentry", Version: "1.0.0", Protocol: 1,
		Capabilities:  []string{"read", "apply", "validate", "outputs", "destroy"},
		CredentialEnv: []string{"SENTRY_AUTH_TOKEN"},
		Inputs: []adapter.Input{
			{Name: "SENTRY_AUTH_TOKEN", Kind: adapter.InputSecret, Required: true, Description: "Sentry organization auth token with project:admin"},
		},
		Outputs: []adapter.Field{
			{Name: "SENTRY_DSN", Sensitivity: "SECRET"},
			{Name: "SENTRY_PROJECT_SLUG", Sensitivity: "PUBLIC"},
		},
	}
}

type project struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type clientKey struct {
	ID  string `json:"id"`
	DSN struct {
		Public string `json:"public"`
	} `json:"dsn"`
}

func (Handler) Handle(ctx context.Context, r adapter.Request) (adapter.Response, error) {
	if err := preflight(r); err != nil {
		return adapter.Response{}, err
	}
	auth := "Bearer " + strings.TrimSpace(os.Getenv("SENTRY_AUTH_TOKEN"))
	org := strings.ToLower(strings.TrimSpace(r.Config["organization"]))
	team := strings.ToLower(strings.TrimSpace(r.Config["team"]))
	name := strings.TrimSpace(r.Config["name"])
	slug := projectSlug(name)
	platform := strings.TrimSpace(r.Config["platform"])
	if platform == "" {
		platform = defaultPlatform
	}

	switch r.Hook {
	case "read", "validate":
		id := r.ExternalID
		if id == "" {
			id = slug
		}
		proj, status, err := getProject(ctx, auth, org, id)
		if status == http.StatusNotFound {
			return adapter.Response{Absent: true}, nil
		}
		if err != nil {
			return adapter.Response{}, classify(status, r.Hook, err)
		}
		return adapter.Response{ExternalID: proj.Slug, Status: "ready"}, nil
	case "apply":
		proj, status, err := getProject(ctx, auth, org, slug)
		if status == http.StatusNotFound {
			proj, status, err = createProject(ctx, auth, org, team, name, slug, platform)
			if err != nil {
				return adapter.Response{}, classify(status, "apply", err)
			}
		} else if err != nil {
			return adapter.Response{}, classify(status, "apply", err)
		}
		if proj.Name != name {
			return adapter.Response{}, adapter.NewError("sentry_project_unmanaged")
		}
		dsn, status, err := ensureDSN(ctx, auth, org, proj.Slug)
		if err != nil {
			return adapter.Response{}, classify(status, "outputs", err)
		}
		return adapter.Response{
			ExternalID: proj.Slug,
			Status:     "ready",
			Outputs: map[string]string{
				"SENTRY_DSN":          dsn,
				"SENTRY_PROJECT_SLUG": proj.Slug,
			},
		}, nil
	case "outputs":
		id := r.ExternalID
		if id == "" {
			id = slug
		}
		proj, status, err := getProject(ctx, auth, org, id)
		if err != nil {
			return adapter.Response{}, classify(status, "outputs", err)
		}
		if proj.Name != name {
			return adapter.Response{}, adapter.NewError("sentry_project_unmanaged")
		}
		dsn, status, err := ensureDSN(ctx, auth, org, proj.Slug)
		if err != nil {
			return adapter.Response{}, classify(status, "outputs", err)
		}
		return adapter.Response{Outputs: map[string]string{
			"SENTRY_DSN":          dsn,
			"SENTRY_PROJECT_SLUG": proj.Slug,
		}}, nil
	case "destroy":
		id := r.ExternalID
		if id == "" {
			id = slug
		}
		proj, status, err := getProject(ctx, auth, org, id)
		if status == http.StatusNotFound {
			return adapter.Response{Absent: true}, nil
		}
		if err != nil {
			return adapter.Response{}, classify(status, "destroy", err)
		}
		if proj.Name != name {
			return adapter.Response{}, adapter.NewError("sentry_project_unmanaged")
		}
		status, err = httpclient.JSON(ctx, http.MethodDelete, apiBase+"/projects/"+url.PathEscape(org)+"/"+url.PathEscape(proj.Slug)+"/", auth, nil, nil)
		if status == http.StatusNotFound || status == http.StatusNoContent || status == http.StatusAccepted {
			return adapter.Response{Absent: true}, nil
		}
		if err != nil {
			return adapter.Response{}, classify(status, "destroy", err)
		}
		return adapter.Response{Absent: true}, nil
	default:
		return adapter.Response{}, adapter.NewError("sentry_invalid_request")
	}
}

func preflight(r adapter.Request) error {
	switch r.Hook {
	case "read", "apply", "validate", "outputs", "destroy":
	default:
		return adapter.NewError("sentry_invalid_request")
	}
	if strings.TrimSpace(os.Getenv("SENTRY_AUTH_TOKEN")) == "" {
		return adapter.NewError("sentry_credential_missing")
	}
	org := strings.ToLower(strings.TrimSpace(r.Config["organization"]))
	if !orgSlugPattern.MatchString(org) {
		return adapter.NewError("sentry_organization_required")
	}
	team := strings.ToLower(strings.TrimSpace(r.Config["team"]))
	if !teamSlugPattern.MatchString(team) {
		return adapter.NewError("sentry_team_required")
	}
	if platform := strings.TrimSpace(r.Config["platform"]); platform != "" && !platformPattern.MatchString(platform) {
		return adapter.NewError("sentry_platform_invalid")
	}
	switch r.Hook {
	case "apply", "outputs", "destroy":
		if strings.TrimSpace(r.Config["name"]) == "" {
			return adapter.NewError("sentry_name_required")
		}
	case "read", "validate":
		if r.ExternalID == "" && strings.TrimSpace(r.Config["name"]) == "" {
			return adapter.NewError("sentry_name_required")
		}
	}
	return nil
}

func projectSlug(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == '.':
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "envtie"
	}
	if len(slug) > maxSlugLen {
		slug = strings.Trim(slug[:maxSlugLen], "-")
	}
	return slug
}

func getProject(ctx context.Context, auth, org, slug string) (project, int, error) {
	var proj project
	status, err := httpclient.JSON(ctx, http.MethodGet, apiBase+"/projects/"+url.PathEscape(org)+"/"+url.PathEscape(slug)+"/", auth, nil, &proj)
	return proj, status, err
}

func createProject(ctx context.Context, auth, org, team, name, slug, platform string) (project, int, error) {
	var proj project
	body := map[string]any{
		"name":          name,
		"slug":          slug,
		"platform":      platform,
		"default_rules": false,
	}
	status, err := httpclient.JSON(ctx, http.MethodPost, apiBase+"/teams/"+url.PathEscape(org)+"/"+url.PathEscape(team)+"/projects/", auth, body, &proj)
	return proj, status, err
}

func ensureDSN(ctx context.Context, auth, org, slug string) (string, int, error) {
	var keys []clientKey
	status, err := httpclient.JSON(ctx, http.MethodGet, apiBase+"/projects/"+url.PathEscape(org)+"/"+url.PathEscape(slug)+"/keys/", auth, nil, &keys)
	if err != nil {
		return "", status, err
	}
	for _, key := range keys {
		if dsn := strings.TrimSpace(key.DSN.Public); dsn != "" {
			return dsn, status, nil
		}
	}
	var created clientKey
	status, err = httpclient.JSON(ctx, http.MethodPost, apiBase+"/projects/"+url.PathEscape(org)+"/"+url.PathEscape(slug)+"/keys/", auth, map[string]string{"name": "EnvTie"}, &created)
	if err != nil {
		return "", status, err
	}
	dsn := strings.TrimSpace(created.DSN.Public)
	if dsn == "" {
		return "", status, adapter.NewError("sentry_dsn_unavailable")
	}
	return dsn, status, nil
}

func classify(status int, phase string, providerErr error) error {
	if status < 400 && providerErr == nil {
		return adapter.NewError("sentry_unavailable")
	}
	switch httpclient.StatusClass(status) {
	case "auth":
		return adapter.NewError("sentry_unauthorized")
	case "not_found":
		if phase == "outputs" {
			return adapter.NewError("sentry_dsn_unavailable")
		}
		return adapter.NewError("sentry_not_found")
	case "quota":
		return adapter.NewError("sentry_quota")
	case "rate_limit":
		return adapter.NewError("sentry_rate_limited")
	case "invalid":
		return adapter.NewError("sentry_invalid_request")
	case "unavailable":
		return adapter.NewError("sentry_unavailable")
	default:
		if providerErr != nil && status == 0 {
			return adapter.NewError("sentry_unavailable")
		}
		return adapter.NewError("sentry_invalid_request")
	}
}
