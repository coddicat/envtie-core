package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/coddicat/envtie-core/adapters/httpclient"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

// ManagedVariable marks a GitHub environment this adapter created. Apply refuses
// an existing environment that does not carry the marker.
const ManagedVariable = "ENVTIE_MANAGED"

var githubAPI = "https://api.github.com"
var environmentName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,62}$`)

type Handler struct{}

func (Handler) Manifest() adapter.Manifest {
	return adapter.Manifest{
		Name: "github", Version: "1.0.0", Protocol: 1,
		Capabilities: []string{"read", "apply", "validate", "outputs", "destroy"},
		// The runner copies only these names into the adapter process.
		// GITHUB_REPOSITORY and GITHUB_SHA are Actions context, not secrets.
		CredentialEnv: []string{"GITHUB_TOKEN", "GITHUB_REPOSITORY", "GITHUB_SHA"},
		Inputs: []adapter.Input{
			{Name: "GITHUB_TOKEN", Kind: adapter.InputSecret, Required: true, Description: "GitHub App installation token"},
			{Name: "GITHUB_REPOSITORY", Kind: adapter.InputContext, Required: true, Description: "Repository owner/name"},
			{Name: "GITHUB_SHA", Kind: adapter.InputContext, Required: false, Description: "Commit supplied by GitHub Actions for a pull request"},
		},
		Outputs: []adapter.Field{
			{Name: "ENVIRONMENT_NAME", Sensitivity: "PUBLIC"},
			{Name: "ENVIRONMENT_URL", Sensitivity: "PUBLIC"},
			{Name: "PR_NUMBER", Sensitivity: "PUBLIC"},
			{Name: "PR_HEAD_SHA", Sensitivity: "PUBLIC"},
		},
	}
}

func (Handler) Handle(ctx context.Context, r adapter.Request) (adapter.Response, error) {
	if r.Config["pull_number"] != "" && r.Hook != "apply" && r.Hook != "destroy" {
		return readPullRequest(ctx, r)
	}
	if err := preflight(r); err != nil {
		return adapter.Response{}, err
	}
	owner, repo, err := splitRepo(r.Config["repository"])
	if err != nil {
		return adapter.Response{}, err
	}
	name := environment(r)
	token := os.Getenv("GITHUB_TOKEN")
	base := githubAPI + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/environments/" + url.PathEscape(name)
	auth := "Bearer " + token
	switch r.Hook {
	case "read", "validate":
		return readEnvironment(ctx, base, auth, name)
	case "apply":
		return applyEnvironment(ctx, base, auth, name, r)
	case "outputs":
		outputs := map[string]string{"ENVIRONMENT_NAME": name}
		if link := environmentURL(r); link != "" {
			outputs["ENVIRONMENT_URL"] = link
		}
		return adapter.Response{ExternalID: name, Status: "ready", Outputs: outputs}, nil
	case "destroy":
		return destroyEnvironment(ctx, base, auth)
	default:
		return adapter.Response{}, adapter.NewError("github_invalid_request")
	}
}

func preflight(r adapter.Request) error {
	switch r.Hook {
	case "read", "apply", "validate", "outputs", "destroy":
	default:
		return adapter.NewError("github_invalid_request")
	}
	if os.Getenv("GITHUB_TOKEN") == "" {
		return adapter.NewError("github_credential_missing")
	}
	if r.Config["repository"] == "" && os.Getenv("GITHUB_REPOSITORY") == "" {
		return adapter.NewError("github_repository_required")
	}
	if environment(r) == "" || !environmentName.MatchString(environment(r)) {
		return adapter.NewError("github_environment_required")
	}
	return nil
}

// environment is the EnvTie environment name, such as pr-17. The runner also
// sets config name to <environment>-<component> so other providers can recover
// a lost create. Using that here would create a second GitHub environment,
// pr-17-github, beside the pr-17 environment the workflow job and secret
// delivery use.
func environment(r adapter.Request) string {
	return r.Environment
}

// environmentURL is the optional preview link; {env} becomes the environment name.
func environmentURL(r adapter.Request) string {
	return strings.ReplaceAll(r.Config["environment_url"], "{env}", r.Environment)
}

func splitRepo(repository string) (string, string, error) {
	if repository == "" {
		repository = os.Getenv("GITHUB_REPOSITORY")
	}
	owner, repo, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", adapter.NewError("github_repository_required")
	}
	return owner, repo, nil
}

func readEnvironment(ctx context.Context, base, auth, name string) (adapter.Response, error) {
	status, err := httpclient.JSON(ctx, http.MethodGet, base, auth, nil, &struct{}{})
	if status == http.StatusNotFound {
		return adapter.Response{Absent: true}, nil
	}
	if err != nil {
		return adapter.Response{}, classify(status, err)
	}
	return adapter.Response{ExternalID: name, Status: "ready"}, nil
}

func applyEnvironment(ctx context.Context, base, auth, name string, r adapter.Request) (adapter.Response, error) {
	status, err := httpclient.JSON(ctx, http.MethodGet, base, auth, nil, &struct{}{})
	existed := status != http.StatusNotFound
	if existed && err != nil {
		return adapter.Response{}, classify(status, err)
	}
	if !existed {
		status, err = httpclient.JSON(ctx, http.MethodPut, base, auth, map[string]any{}, nil)
		if err != nil {
			return adapter.Response{}, classify(status, err)
		}
	} else {
		managed, markerErr := hasMarker(ctx, base, auth)
		if markerErr != nil {
			return adapter.Response{}, markerErr
		}
		if !managed {
			return adapter.Response{}, adapter.NewError("github_environment_unmanaged")
		}
	}
	if err := ensureMarker(ctx, base, auth); err != nil {
		return adapter.Response{}, err
	}
	if r.Config["deployment"] == "true" && deploymentCommit(r) != "" {
		if err := recordDeployment(ctx, base, auth, name, r); err != nil {
			return adapter.Response{}, err
		}
	}
	return adapter.Response{ExternalID: name, Status: "ready"}, nil
}

func destroyEnvironment(ctx context.Context, base, auth string) (adapter.Response, error) {
	status, err := httpclient.JSON(ctx, http.MethodGet, base, auth, nil, &struct{}{})
	if status == http.StatusNotFound {
		return adapter.Response{Absent: true}, nil
	}
	if err != nil {
		return adapter.Response{}, classify(status, err)
	}
	managed, err := hasMarker(ctx, base, auth)
	if err != nil {
		return adapter.Response{}, err
	}
	if !managed {
		return adapter.Response{}, adapter.NewError("github_environment_unmanaged")
	}
	status, err = httpclient.JSON(ctx, http.MethodDelete, base, auth, nil, nil)
	if status == http.StatusNotFound || status == http.StatusNoContent {
		return adapter.Response{Absent: true}, nil
	}
	if err != nil {
		return adapter.Response{}, classify(status, err)
	}
	return adapter.Response{Absent: true}, nil
}

func hasMarker(ctx context.Context, base, auth string) (bool, error) {
	var page struct {
		Variables []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"variables"`
	}
	status, err := httpclient.JSON(ctx, http.MethodGet, base+"/variables?per_page=100", auth, nil, &page)
	if status == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, classify(status, err)
	}
	for _, variable := range page.Variables {
		if variable.Name == ManagedVariable && variable.Value == "true" {
			return true, nil
		}
	}
	return false, nil
}

func ensureMarker(ctx context.Context, base, auth string) error {
	body := map[string]string{"name": ManagedVariable, "value": "true"}
	status, err := httpclient.JSON(ctx, http.MethodPatch, base+"/variables/"+ManagedVariable, auth, body, nil)
	if status == http.StatusNotFound {
		status, err = httpclient.JSON(ctx, http.MethodPost, base+"/variables", auth, body, nil)
	}
	if status == http.StatusNoContent || status == http.StatusCreated || status == http.StatusOK {
		return nil
	}
	if err != nil {
		return classify(status, err)
	}
	return nil
}

func deploymentCommit(r adapter.Request) string {
	if sha := strings.TrimSpace(r.Config["head_sha"]); sha != "" {
		return sha
	}
	return strings.TrimSpace(os.Getenv("GITHUB_SHA"))
}

func recordDeployment(ctx context.Context, environmentAPI, auth, name string, r adapter.Request) error {
	sha := deploymentCommit(r)
	if sha == "" {
		return adapter.NewError("github_sha_required")
	}
	repoURL := strings.TrimSuffix(environmentAPI, "/environments/"+url.PathEscape(name))
	var created struct {
		ID int64 `json:"id"`
	}
	status, err := httpclient.JSON(ctx, http.MethodPost, repoURL+"/deployments", auth, map[string]any{
		"ref": nameRef(sha), "environment": name, "auto_merge": false,
		"required_contexts": []string{}, "transient_environment": true,
		"description": "Envtie pull-request preview",
	}, &created)
	if err != nil || created.ID <= 0 {
		return classify(status, err)
	}
	state := map[string]any{"state": "in_progress", "environment": name, "description": "Envtie preview"}
	if link := environmentURL(r); link != "" {
		state["environment_url"] = link
		state["state"] = "success"
	}
	status, err = httpclient.JSON(ctx, http.MethodPost, fmt.Sprintf("%s/deployments/%d/statuses", repoURL, created.ID), auth, state, nil)
	if err != nil {
		return classify(status, err)
	}
	return nil
}

func nameRef(sha string) string {
	return sha
}

func classify(status int, err error) error {
	if status == http.StatusTooManyRequests || httpclient.ErrorContains(err, "rate limit") {
		return adapter.NewError("github_rate_limited")
	}
	switch httpclient.StatusClass(status) {
	case "auth":
		return adapter.NewError("github_unauthorized")
	case "not_found":
		return adapter.NewError("github_unavailable")
	default:
		if err == nil {
			return adapter.NewError("github_unavailable")
		}
		return adapter.NewError("github_unavailable")
	}
}

func readPullRequest(ctx context.Context, r adapter.Request) (adapter.Response, error) {
	if os.Getenv("GITHUB_TOKEN") == "" {
		return adapter.Response{}, adapter.NewError("github_credential_missing")
	}
	owner, repo, err := splitRepo(r.Config["repository"])
	if err != nil {
		return adapter.Response{}, err
	}
	pr := r.Config["pull_number"]
	if pr == "" {
		pr = os.Getenv("PR_NUMBER")
	}
	if pr == "" {
		return adapter.Response{}, adapter.NewError("github_invalid_request")
	}
	var out struct {
		Number int `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	endpoint := githubAPI + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/pulls/" + url.PathEscape(pr)
	status, err := httpclient.JSON(ctx, http.MethodGet, endpoint, "Bearer "+os.Getenv("GITHUB_TOKEN"), nil, &out)
	if err != nil {
		return adapter.Response{}, classify(status, err)
	}
	return adapter.Response{ExternalID: fmt.Sprint(out.Number), Status: "ready", Outputs: map[string]string{"PR_NUMBER": fmt.Sprint(out.Number), "PR_HEAD_SHA": out.Head.SHA}}, nil
}
