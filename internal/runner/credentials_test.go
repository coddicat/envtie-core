package runner

import (
	"context"
	"testing"

	"github.com/coddicat/envtie-core/core"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

func manifestCall(manifests map[string]adapter.Manifest) CallFunc {
	return func(_ context.Context, c core.Component, req adapter.Request) (adapter.Response, error) {
		if req.Hook != "manifest" {
			return adapter.Response{}, adapter.NewError("unexpected_hook")
		}
		m, ok := manifests[c.Adapter]
		if !ok {
			return adapter.Response{}, adapter.NewError("unknown_adapter")
		}
		return adapter.Response{Manifest: &m}, nil
	}
}

func TestCredentialGapsCollectsMissingInputsBeforeApply(t *testing.T) {
	t.Setenv("UPSTASH_EMAIL", "person@example.com")
	t.Setenv("UPSTASH_API_KEY", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_SHA", "")
	engine := Engine{Call: manifestCall(map[string]adapter.Manifest{
		"upstash@1": {Name: "upstash", Version: "1.0.0", Protocol: 1, Capabilities: []string{"read", "apply", "validate", "outputs", "destroy"}, Inputs: []adapter.Input{
			{Name: "UPSTASH_EMAIL", Kind: adapter.InputVariable, Required: true},
			{Name: "UPSTASH_API_KEY", Kind: adapter.InputSecret, Required: true},
		}},
		"github@1": {Name: "github", Version: "1.0.0", Protocol: 1, Capabilities: []string{"read", "apply", "validate", "outputs", "destroy"}, Inputs: []adapter.Input{
			{Name: "GITHUB_TOKEN", Kind: adapter.InputSecret, Required: true},
			{Name: "GITHUB_REPOSITORY", Kind: adapter.InputContext, Required: true},
			{Name: "GITHUB_SHA", Kind: adapter.InputContext, Required: false},
		}},
	})}
	actions := []core.Action{
		{Component: core.Component{ID: "cache", Adapter: "upstash@1", Ownership: core.Managed}},
		{Component: core.Component{ID: "github", Adapter: "github@1", Ownership: core.Managed, Config: map[string]string{"deployment": "true"}}},
	}
	gaps, err := engine.CredentialGaps(context.Background(), actions, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, gap := range gaps {
		got[gap.Name] = gap.Kind
	}
	for _, name := range []string{"UPSTASH_API_KEY", "GITHUB_TOKEN", "GITHUB_REPOSITORY", "GITHUB_SHA"} {
		if _, ok := got[name]; !ok {
			t.Fatalf("missing %s in %v", name, got)
		}
	}
	if _, ok := got["UPSTASH_EMAIL"]; ok {
		t.Fatal("present variable was requested")
	}
	if got["UPSTASH_API_KEY"] != adapter.InputSecret || got["GITHUB_REPOSITORY"] != adapter.InputContext {
		t.Fatalf("kinds: %v", got)
	}
}

func TestCredentialGapsAddsGitHubDeliveryWithoutAGitHubComponent(t *testing.T) {
	t.Setenv("UPSTASH_EMAIL", "person@example.com")
	t.Setenv("UPSTASH_API_KEY", "key")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_REPOSITORY", "acme/shop")
	engine := Engine{Call: manifestCall(map[string]adapter.Manifest{
		"upstash@1": {Name: "upstash", Version: "1.0.0", Protocol: 1, Capabilities: []string{"read", "apply", "validate", "outputs", "destroy"}, CredentialEnv: []string{"UPSTASH_EMAIL", "UPSTASH_API_KEY"}},
	})}
	actions := []core.Action{{Component: core.Component{ID: "cache", Adapter: "upstash@1", Ownership: core.Managed}}}
	bindings := []core.Binding{{From: "cache.REDIS_URL", To: "github.environment.secret.REDIS_URL"}}
	gaps, err := engine.CredentialGaps(context.Background(), actions, bindings, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 1 || gaps[0].Name != "GITHUB_TOKEN" {
		t.Fatalf("gaps: %+v", gaps)
	}
	destroy, err := engine.CredentialGaps(context.Background(), actions, bindings, false)
	if err != nil || len(destroy) != 0 {
		t.Fatalf("destroy gaps: %+v %v", destroy, err)
	}
}

func TestCredentialGapsRejectsAnUndeclaredName(t *testing.T) {
	engine := Engine{Call: manifestCall(map[string]adapter.Manifest{
		"example@1": {Name: "example", Version: "1.0.0", Protocol: 1, Capabilities: []string{"read", "apply", "validate", "outputs", "destroy"}, Inputs: []adapter.Input{
			{Name: "lower_case", Kind: adapter.InputSecret, Required: true},
		}},
	})}
	_, err := engine.CredentialGaps(context.Background(), []core.Action{{Component: core.Component{ID: "custom", Adapter: "example@1", Ownership: core.Managed}}}, nil, false)
	if err == nil {
		t.Fatal("expected an invalid declaration")
	}
}
