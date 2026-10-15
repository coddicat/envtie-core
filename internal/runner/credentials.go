package runner

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/coddicat/envtie-core/core"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

// CredentialGaps reads every adapter manifest and returns the required values
// that are absent from this process. It does not change the environment.
// includeDelivery adds the GitHub token and repository when a binding writes
// to a GitHub Environment.
func (e Engine) CredentialGaps(ctx context.Context, actions []core.Action, bindings []core.Binding, includeDelivery bool) ([]adapter.Input, error) {
	var missing []adapter.Input
	seen := map[string]bool{}
	add := func(in adapter.Input) {
		if seen[in.Name] {
			return
		}
		seen[in.Name] = true
		if strings.TrimSpace(os.Getenv(in.Name)) != "" {
			return
		}
		missing = append(missing, in)
	}
	for _, action := range actions {
		manifest, err := e.verifyAdapter(ctx, action.Component)
		if err != nil {
			return nil, err
		}
		for _, in := range manifest.RuntimeInputs() {
			if !validCredentialName(in.Name) || !validCredentialKind(in.Kind) {
				return nil, fmt.Errorf("%s: invalid credential declaration", action.Component.ID)
			}
			if in.Name == "GITHUB_REPOSITORY" && strings.TrimSpace(action.Component.Config["repository"]) != "" {
				continue
			}
			if in.Name == "UPSTASH_EMAIL" && strings.TrimSpace(action.Component.Config["email"]) != "" {
				continue
			}
			if in.Name == "GITHUB_SHA" || !in.Required {
				continue
			}
			add(in)
		}
	}
	if includeDelivery {
		repository := ""
		for _, action := range actions {
			if repo := strings.TrimSpace(action.Component.Config["repository"]); repo != "" {
				repository = repo
				break
			}
		}
		for _, binding := range bindings {
			if _, _, ok := core.GitHubBinding(binding.To); !ok {
				continue
			}
			add(adapter.Input{Name: "GITHUB_TOKEN", Kind: adapter.InputSecret, Required: true, Description: "GitHub App installation token"})
			if repository == "" {
				add(adapter.Input{Name: "GITHUB_REPOSITORY", Kind: adapter.InputContext, Required: true, Description: "Repository owner/name"})
			}
			break
		}
	}
	return missing, nil
}

func validCredentialName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func validCredentialKind(kind string) bool {
	switch kind {
	case adapter.InputSecret, adapter.InputVariable, adapter.InputContext:
		return true
	default:
		return false
	}
}
