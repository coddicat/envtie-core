package upstash

import (
	"context"
	"net/url"
	"os"
	"strconv"
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
			{Name: "UPSTASH_EMAIL", Kind: adapter.InputVariable, Required: true, Description: "Upstash account email. Prefer config email; this variable is only a fallback."},
			{Name: "UPSTASH_API_KEY", Kind: adapter.InputSecret, Required: true, Description: "Upstash API key"},
		},
		Outputs: []adapter.Field{
			{Name: "REDIS_URL", Sensitivity: "SECRET"},
			{Name: "UPSTASH_REDIS_REST_URL", Sensitivity: "PUBLIC"},
			{Name: "UPSTASH_REDIS_REST_TOKEN", Sensitivity: "SECRET"},
			{Name: "UPSTASH_VECTOR_REST_URL", Sensitivity: "PUBLIC"},
			{Name: "UPSTASH_VECTOR_REST_TOKEN", Sensitivity: "SECRET"},
		},
	}
}

func (Handler) Handle(ctx context.Context, r adapter.Request) (adapter.Response, error) {
	// Required credentials and config are rejected before any provider call.
	// The runner's read-before-create is the non-mutating preflight: apply is
	// not attempted when that read fails.
	if err := preflight(r); err != nil {
		return adapter.Response{}, err
	}
	email, key := accountEmail(r), os.Getenv("UPSTASH_API_KEY")
	auth := "Basic " + basic(email, key)
	switch product(r.Config) {
	case "vector":
		return handleVector(ctx, r, auth)
	default:
		return handleRedis(ctx, r, auth)
	}
}

func handleRedis(ctx context.Context, r adapter.Request, auth string) (adapter.Response, error) {
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
		return handleRedis(ctx, adapter.Request{Hook: "read", Config: r.Config, ExternalID: r.ExternalID}, auth)
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

func handleVector(ctx context.Context, r adapter.Request, auth string) (adapter.Response, error) {
	base := "https://api.upstash.com/v2/vector/index"
	switch r.Hook {
	case "read":
		if r.ExternalID != "" {
			var x struct {
				ID string `json:"id"`
			}
			status, err := httpclient.JSON(ctx, "GET", base+"/"+url.PathEscape(r.ExternalID), auth, nil, &x)
			if status == 404 {
				return adapter.Response{Absent: true}, nil
			}
			if err != nil {
				return adapter.Response{}, classify(status, "read", err)
			}
			return adapter.Response{ExternalID: x.ID, Status: "ready"}, nil
		}
		var xs []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		status, err := httpclient.JSON(ctx, "GET", base, auth, nil, &xs)
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
		body, err := vectorCreateBody(r.Config)
		if err != nil {
			return adapter.Response{}, err
		}
		var x struct {
			ID string `json:"id"`
		}
		status, err := httpclient.JSON(ctx, "POST", base, auth, body, &x)
		if err != nil {
			return adapter.Response{}, classify(status, "apply", err)
		}
		if x.ID == "" {
			return adapter.Response{}, adapter.NewError("upstash_unavailable")
		}
		return adapter.Response{ExternalID: x.ID, Status: "created"}, nil
	case "validate":
		return handleVector(ctx, adapter.Request{Hook: "read", Config: r.Config, ExternalID: r.ExternalID}, auth)
	case "outputs":
		var x struct {
			Endpoint string `json:"endpoint"`
			Token    string `json:"token"`
		}
		status, err := httpclient.JSON(ctx, "GET", base+"/"+url.PathEscape(r.ExternalID), auth, nil, &x)
		if err != nil {
			return adapter.Response{}, classify(status, "outputs", err)
		}
		if x.Endpoint == "" || x.Token == "" {
			return adapter.Response{}, adapter.NewError("upstash_outputs_unavailable")
		}
		host := x.Endpoint
		if strings.HasPrefix(host, "https://") || strings.HasPrefix(host, "http://") {
			return adapter.Response{Outputs: map[string]string{
				"UPSTASH_VECTOR_REST_URL":   strings.TrimRight(host, "/"),
				"UPSTASH_VECTOR_REST_TOKEN": x.Token,
			}}, nil
		}
		if !strings.Contains(host, ".") {
			host += ".upstash.io"
		}
		return adapter.Response{Outputs: map[string]string{
			"UPSTASH_VECTOR_REST_URL":   "https://" + host,
			"UPSTASH_VECTOR_REST_TOKEN": x.Token,
		}}, nil
	case "destroy":
		status, err := httpclient.JSON(ctx, "DELETE", base+"/"+url.PathEscape(r.ExternalID), auth, nil, nil)
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

func product(cfg map[string]string) string {
	switch strings.ToLower(strings.TrimSpace(cfg["product"])) {
	case "", "redis":
		return "redis"
	case "vector":
		return "vector"
	default:
		return ""
	}
}

func vectorCreateBody(cfg map[string]string) (map[string]any, error) {
	region := strings.TrimSpace(cfg["region"])
	if region == "" {
		region = strings.TrimSpace(cfg["primary_region"])
	}
	if region == "" {
		region = "us-east-1"
	}
	similarity := strings.ToUpper(strings.TrimSpace(cfg["similarity_function"]))
	if similarity == "" {
		similarity = "COSINE"
	}
	switch similarity {
	case "COSINE", "EUCLIDEAN", "DOT_PRODUCT":
	default:
		return nil, adapter.NewError("upstash_similarity_invalid")
	}
	dimRaw := strings.TrimSpace(cfg["dimension_count"])
	if dimRaw == "" {
		return nil, adapter.NewError("upstash_dimension_required")
	}
	dim, err := strconv.Atoi(dimRaw)
	if err != nil || dim < 1 || dim > 65536 {
		return nil, adapter.NewError("upstash_dimension_required")
	}
	indexType := strings.ToUpper(strings.TrimSpace(cfg["index_type"]))
	if indexType == "" {
		indexType = "DENSE"
	}
	switch indexType {
	case "DENSE", "SPARSE", "HYBRID":
	default:
		return nil, adapter.NewError("upstash_invalid_request")
	}
	plan := strings.ToLower(strings.TrimSpace(cfg["type"]))
	if plan == "" {
		plan = "payg"
	}
	body := map[string]any{
		"name":                cfg["name"],
		"region":              region,
		"similarity_function": similarity,
		"dimension_count":     dim,
		"index_type":          indexType,
		"type":                plan,
	}
	if model := strings.TrimSpace(cfg["embedding_model"]); model != "" {
		body["embedding_model"] = model
	}
	if model := strings.TrimSpace(cfg["sparse_embedding_model"]); model != "" {
		body["sparse_embedding_model"] = model
	}
	return body, nil
}

func accountEmail(r adapter.Request) string {
	if email := strings.TrimSpace(r.Config["email"]); email != "" {
		return email
	}
	return strings.TrimSpace(os.Getenv("UPSTASH_EMAIL"))
}

func preflight(r adapter.Request) error {
	switch r.Hook {
	case "read", "apply", "validate", "outputs", "destroy":
	default:
		return adapter.NewError("upstash_invalid_request")
	}
	if accountEmail(r) == "" || os.Getenv("UPSTASH_API_KEY") == "" {
		return adapter.NewError("upstash_credential_missing")
	}
	if product(r.Config) == "" {
		return adapter.NewError("upstash_product_invalid")
	}
	switch r.Hook {
	case "apply":
		if r.Config["name"] == "" {
			return adapter.NewError("upstash_name_required")
		}
		if product(r.Config) == "vector" {
			if _, err := vectorCreateBody(r.Config); err != nil {
				return err
			}
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
