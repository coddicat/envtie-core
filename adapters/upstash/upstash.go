package upstash

import (
	"context"
	"net/url"
	"os"
	"strings"

	"github.com/coddicat/envtie-core/adapters/httpclient"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

type Handler struct{}

func (Handler) Manifest() adapter.Manifest {
	return adapter.Manifest{
		Name: "upstash", Version: "1.0.0", Protocol: 1,
		Capabilities:  []string{"read", "apply", "validate", "outputs", "destroy"},
		CredentialEnv: []string{"UPSTASH_EMAIL", "UPSTASH_API_KEY"},
		Inputs: []adapter.Input{
			{Name: "UPSTASH_EMAIL", Kind: adapter.InputVariable, Required: true, Description: "Upstash account email"},
			{Name: "UPSTASH_API_KEY", Kind: adapter.InputSecret, Required: true, Description: "Upstash API key"},
		},
		Outputs: []adapter.Field{{Name: "REDIS_URL", Sensitivity: "SECRET"}, {Name: "UPSTASH_REDIS_REST_URL", Sensitivity: "PUBLIC"}, {Name: "UPSTASH_REDIS_REST_TOKEN", Sensitivity: "SECRET"}},
	}
}

func (Handler) Handle(ctx context.Context, r adapter.Request) (adapter.Response, error) {
	// Required credentials and config are rejected before any provider call.
	// The runner's read-before-create is the non-mutating preflight: apply is
	// not attempted when that read fails.
	if err := preflight(r); err != nil {
		return adapter.Response{}, err
	}
	email, key := os.Getenv("UPSTASH_EMAIL"), os.Getenv("UPSTASH_API_KEY")
	auth := "Basic " + basic(email, key)
	base := "https://api.upstash.com/v2/redis"
	switch r.Hook {
	case "read":
		if r.ExternalID != "" {
			var x struct {
				ID string `json:"database_id"`
			}
			status, err := httpclient.JSON(ctx, "GET", base+"/database/"+url.PathEscape(r.ExternalID)+"?credentials=hide", auth, nil, &x)
			if status == 404 {
				return adapter.Response{Absent: true}, nil
			}
			if err != nil {
				return adapter.Response{}, classify(status, "read", err)
			}
			return adapter.Response{ExternalID: x.ID, Status: "ready"}, nil
		}
		var xs []struct {
			ID   string `json:"database_id"`
			Name string `json:"database_name"`
		}
		status, err := httpclient.JSON(ctx, "GET", base+"/databases?credentials=hide", auth, nil, &xs)
		if err != nil {
			return adapter.Response{}, classify(status, "read", err)
		}
		for _, x := range xs {
			if x.Name == r.Config["name"] {
				return adapter.Response{ExternalID: x.ID, Status: "ready"}, nil
			}
		}
		return adapter.Response{Absent: true}, nil
	case "apply":
		region := r.Config["primary_region"]
		if region == "" {
			region = "us-east-1"
		}
		platform := r.Config["platform"]
		if platform == "" {
			platform = "aws"
		}
		// Live creates use database_name, platform, and primary_region.
		// A 400 from that call was an account quota, not an incompatible body.
		body := map[string]any{"database_name": r.Config["name"], "platform": platform, "primary_region": region}
		var x struct {
			ID string `json:"database_id"`
		}
		status, err := httpclient.JSON(ctx, "POST", base+"/database", auth, body, &x)
		if err != nil {
			return adapter.Response{}, classify(status, "apply", err)
		}
		return adapter.Response{ExternalID: x.ID, Status: "created"}, nil
	case "validate":
		return (Handler{}).Handle(ctx, adapter.Request{Hook: "read", Config: r.Config, ExternalID: r.ExternalID})
	case "outputs":
		var x struct {
			Endpoint  string `json:"endpoint"`
			Password  string `json:"password"`
			RestToken string `json:"rest_token"`
			RestURL   string `json:"rest_url"`
		}
		status, err := httpclient.JSON(ctx, "GET", base+"/database/"+url.PathEscape(r.ExternalID), auth, nil, &x)
		if err != nil {
			return adapter.Response{}, classify(status, "outputs", err)
		}
		if x.Endpoint == "" || x.Password == "" {
			return adapter.Response{}, adapter.NewError("upstash_outputs_unavailable")
		}
		host := x.Endpoint
		if !strings.Contains(host, ".") {
			host += ".upstash.io"
		}
		redisURL := (&url.URL{Scheme: "rediss", User: url.UserPassword("", x.Password), Host: host + ":6379"}).String()
		restURL := x.RestURL
		if restURL == "" {
			restURL = "https://" + host
		}
		outputs := map[string]string{"REDIS_URL": redisURL, "UPSTASH_REDIS_REST_URL": restURL}
		if x.RestToken != "" {
			outputs["UPSTASH_REDIS_REST_TOKEN"] = x.RestToken
		}
		return adapter.Response{Outputs: outputs}, nil
	case "destroy":
		status, err := httpclient.JSON(ctx, "DELETE", base+"/database/"+url.PathEscape(r.ExternalID), auth, nil, nil)
		if status == 404 {
			return adapter.Response{Absent: true}, nil
		}
		if err != nil {
			return adapter.Response{}, classify(status, "destroy", err)
		}
		return adapter.Response{Absent: true}, nil
	}
	return adapter.Response{}, adapter.NewError("upstash_invalid_request")
}

func preflight(r adapter.Request) error {
	switch r.Hook {
	case "read", "apply", "validate", "outputs", "destroy":
	default:
		return adapter.NewError("upstash_invalid_request")
	}
	if os.Getenv("UPSTASH_EMAIL") == "" || os.Getenv("UPSTASH_API_KEY") == "" {
		return adapter.NewError("upstash_credential_missing")
	}
	switch r.Hook {
	case "apply":
		if r.Config["name"] == "" {
			return adapter.NewError("upstash_name_required")
		}
	case "read", "validate":
		if r.ExternalID == "" && r.Config["name"] == "" {
			return adapter.NewError("upstash_name_required")
		}
	case "outputs":
		if r.ExternalID == "" {
			return adapter.NewError("upstash_outputs_unavailable")
		}
	case "destroy":
		if r.ExternalID == "" {
			return adapter.NewError("upstash_invalid_request")
		}
	}
	return nil
}

func classify(status int, phase string, providerErr error) error {
	// Upstash uses 400 both for malformed requests and for account limits.
	// Recognize only its stable quota message locally; never surface the body.
	if status == 402 ||
		(status == 400 && phase == "apply" &&
			(httpclient.ErrorContains(providerErr, "cannot have more than") ||
				httpclient.ErrorContains(providerErr, "payment method"))) {
		return adapter.NewError("upstash_quota")
	}
	if status < 400 {
		return adapter.NewError("upstash_unavailable")
	}
	switch httpclient.StatusClass(status) {
	case "auth":
		return adapter.NewError("upstash_unauthorized")
	case "not_found":
		if phase == "outputs" {
			return adapter.NewError("upstash_outputs_unavailable")
		}
		return adapter.NewError("upstash_not_found")
	case "quota":
		return adapter.NewError("upstash_quota")
	case "rate_limit":
		return adapter.NewError("upstash_rate_limited")
	case "invalid":
		return adapter.NewError("upstash_invalid_request")
	case "unavailable":
		return adapter.NewError("upstash_unavailable")
	default:
		return adapter.NewError("upstash_invalid_request")
	}
}
