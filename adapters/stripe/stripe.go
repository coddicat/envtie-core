// Package stripe manages one Stripe webhook endpoint per environment and
// delivers its signing secret, which Stripe reveals only when the endpoint is
// created.
package stripe

import (
	"context"
	"errors"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/coddicat/envtie-core/adapters/httpclient"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

var base = "https://api.stripe.com/v1/webhook_endpoints"

const (
	defaultPath = "/api/webhooks/stripe"
	maxPages    = 20
)

var (
	hostPattern       = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	pathPattern       = regexp.MustCompile(`^/[A-Za-z0-9._~/-]{0,200}$`)
	eventPattern      = regexp.MustCompile(`^(?:\*|[a-z0-9_]+(?:\.[a-z0-9_]+)+)$`)
	apiVersionPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(?:\.[a-z]+)?$`)
)

type Handler struct{}

func (Handler) Manifest() adapter.Manifest {
	return adapter.Manifest{Name: "stripe", Version: "1.0.0", Protocol: 1, Capabilities: []string{"read", "apply", "validate", "outputs", "destroy"}, CredentialEnv: []string{"STRIPE_SECRET_KEY"}, Outputs: []adapter.Field{
		{Name: "STRIPE_WEBHOOK_SECRET", Sensitivity: "SECRET", Availability: adapter.AvailabilityCreate},
		{Name: "STRIPE_WEBHOOK_ENDPOINT_ID", Sensitivity: "PUBLIC"},
	}}
}

type endpoint struct {
	ID       string            `json:"id"`
	Secret   string            `json:"secret"`
	Status   string            `json:"status"`
	Metadata map[string]string `json:"metadata"`
}

func (Handler) Handle(ctx context.Context, r adapter.Request) (adapter.Response, error) {
	key := os.Getenv("STRIPE_SECRET_KEY")
	if err := checkKey(key, r.Config["allow_live"] == "true"); err != nil {
		return adapter.Response{}, err
	}
	auth := "Bearer " + key
	switch r.Hook {
	case "read":
		if r.ExternalID != "" {
			ep, found, err := get(ctx, auth, r.ExternalID)
			if err != nil {
				return adapter.Response{}, err
			}
			if !found {
				return adapter.Response{Absent: true}, nil
			}
			return adapter.Response{ExternalID: ep.ID, Status: "ready"}, nil
		}
		name := r.Config["name"]
		if name == "" {
			return adapter.Response{Absent: true}, nil
		}
		ep, err := findByName(ctx, auth, name)
		if err != nil {
			return adapter.Response{}, err
		}
		if ep == nil {
			return adapter.Response{Absent: true}, nil
		}
		return adapter.Response{ExternalID: ep.ID, Status: "ready"}, nil
	case "apply":
		values, err := createForm(r)
		if err != nil {
			return adapter.Response{}, err
		}
		var ep endpoint
		if err := create(ctx, auth, r.IdempotencyKey, values, &ep); err != nil {
			return adapter.Response{}, err
		}
		if ep.ID == "" {
			return adapter.Response{}, errors.New("Stripe returned no endpoint id")
		}
		outputs := map[string]string{"STRIPE_WEBHOOK_ENDPOINT_ID": ep.ID}
		if ep.Secret != "" {
			outputs["STRIPE_WEBHOOK_SECRET"] = ep.Secret
		}
		return adapter.Response{ExternalID: ep.ID, Status: "created", Outputs: outputs}, nil
	case "validate":
		ep, found, err := get(ctx, auth, r.ExternalID)
		if err != nil {
			return adapter.Response{}, err
		}
		if !found {
			return adapter.Response{Absent: true}, nil
		}
		if ep.Status == "disabled" {
			return adapter.Response{}, adapter.NewError("stripe_endpoint_disabled")
		}
		return adapter.Response{ExternalID: ep.ID, Status: "ready"}, nil
	case "outputs":
		ep, found, err := get(ctx, auth, r.ExternalID)
		if err != nil {
			return adapter.Response{}, err
		}
		if !found {
			return adapter.Response{}, adapter.NewError("stripe_endpoint_missing")
		}
		return adapter.Response{Outputs: map[string]string{"STRIPE_WEBHOOK_ENDPOINT_ID": ep.ID}}, nil
	case "destroy":
		status, err := httpclient.Form(ctx, "DELETE", base+"/"+url.PathEscape(r.ExternalID), auth, nil, nil, nil)
		if status == 404 {
			return adapter.Response{Absent: true}, nil
		}
		if err != nil {
			return adapter.Response{}, classify(err)
		}
		return adapter.Response{Absent: true}, nil
	}
	return adapter.Response{}, errors.New("unsupported hook")
}

// checkKey refuses live-mode keys unless the blueprint opts in explicitly, so a
// preview environment cannot register endpoints on a production account by accident.
func checkKey(key string, allowLive bool) error {
	switch {
	case key == "":
		return adapter.NewError("stripe_key_missing")
	case strings.HasPrefix(key, "sk_test_"), strings.HasPrefix(key, "rk_test_"):
		return nil
	case strings.HasPrefix(key, "sk_live_"), strings.HasPrefix(key, "rk_live_"):
		if allowLive {
			return nil
		}
		return adapter.NewError("stripe_live_key_refused")
	}
	return adapter.NewError("stripe_key_invalid")
}

func createForm(r adapter.Request) (url.Values, error) {
	name := r.Config["name"]
	if name == "" {
		return nil, adapter.NewError("stripe_name_required")
	}
	host := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(r.Config["host"]), "{env}", r.Environment))
	if !hostPattern.MatchString(host) {
		return nil, adapter.NewError("stripe_host_invalid")
	}
	path := r.Config["path"]
	if path == "" {
		path = defaultPath
	}
	if !pathPattern.MatchString(path) {
		return nil, adapter.NewError("stripe_path_invalid")
	}
	values := url.Values{}
	values.Set("url", "https://"+host+path)
	events := 0
	for _, e := range strings.Split(r.Config["events"], ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !eventPattern.MatchString(e) {
			return nil, adapter.NewError("stripe_event_invalid")
		}
		values.Add("enabled_events[]", e)
		events++
	}
	if events == 0 {
		return nil, adapter.NewError("stripe_events_required")
	}
	if v := r.Config["api_version"]; v != "" {
		if !apiVersionPattern.MatchString(v) {
			return nil, adapter.NewError("stripe_api_version_invalid")
		}
		values.Set("api_version", v)
	}
	description := r.Config["description"]
	if description == "" {
		description = "EnvTie.dev " + r.Environment
	}
	if len(description) > 500 {
		description = description[:500]
	}
	values.Set("description", description)
	values.Set("metadata[envtie_name]", name)
	values.Set("metadata[envtie_environment]", r.Environment)
	values.Set("metadata[envtie_component]", r.Component)
	return values, nil
}

// create retries with the same Idempotency-Key so Stripe replays the original
// response, which is the only place the signing secret ever appears.
func create(ctx context.Context, auth, idempotencyKey string, values url.Values, target *endpoint) error {
	headers := map[string]string{}
	if idempotencyKey != "" {
		headers["Idempotency-Key"] = idempotencyKey
	}
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var status int
		status, err = httpclient.Form(ctx, "POST", base, auth, values, headers, target)
		if err == nil {
			return nil
		}
		if idempotencyKey == "" || (status != 0 && status != 409 && status != 429 && status < 500) {
			return classify(err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * retryDelay):
		}
	}
	return classify(err)
}

var retryDelay = time.Second

func get(ctx context.Context, auth, id string) (endpoint, bool, error) {
	var ep endpoint
	if id == "" {
		return ep, false, nil
	}
	status, err := httpclient.Form(ctx, "GET", base+"/"+url.PathEscape(id), auth, nil, nil, &ep)
	if status == 404 {
		return ep, false, nil
	}
	if err != nil {
		return ep, false, classify(err)
	}
	return ep, true, nil
}

func findByName(ctx context.Context, auth, name string) (*endpoint, error) {
	after := ""
	for page := 0; page < maxPages; page++ {
		q := url.Values{"limit": {"100"}}
		if after != "" {
			q.Set("starting_after", after)
		}
		var list struct {
			Data    []endpoint `json:"data"`
			HasMore bool       `json:"has_more"`
		}
		if _, err := httpclient.Form(ctx, "GET", base+"?"+q.Encode(), auth, nil, nil, &list); err != nil {
			return nil, classify(err)
		}
		for i := range list.Data {
			if list.Data[i].Metadata["envtie_name"] == name {
				return &list.Data[i], nil
			}
		}
		if !list.HasMore || len(list.Data) == 0 {
			return nil, nil
		}
		after = list.Data[len(list.Data)-1].ID
	}
	return nil, adapter.NewError("stripe_list_too_large")
}

func classify(err error) error {
	var pe *httpclient.ProviderError
	if !errors.As(err, &pe) {
		return err
	}
	switch pe.Status {
	case 401:
		return adapter.NewError("stripe_unauthorized")
	case 403:
		return adapter.NewError("stripe_permission_denied")
	case 429:
		return adapter.NewError("stripe_rate_limited")
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if httpclient.DecodeErrorBody(pe, &body) {
		msg := strings.ToLower(body.Error.Message)
		if strings.Contains(msg, "maximum") && strings.Contains(msg, "endpoint") {
			return adapter.NewError("stripe_endpoint_limit")
		}
		if strings.Contains(msg, "url") && pe.Status == 400 {
			return adapter.NewError("stripe_url_rejected")
		}
	}
	return err
}
