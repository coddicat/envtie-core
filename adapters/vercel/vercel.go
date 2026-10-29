// Package vercel adopts an existing Vercel project and maps each EnvTie
// environment to one git branch: branch-scoped preview env vars plus a preview
// deployment built by Vercel from git. The runner never builds code or pushes
// images. The project itself is never created or deleted.
package vercel

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coddicat/envtie-core/adapters/httpclient"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

var (
	apiBase      = "https://api.vercel.com"
	pollInterval = 3 * time.Second
	pollTimeout  = 10 * time.Minute
	projectIDPat = regexp.MustCompile(`^[A-Za-z0-9._-]{1,256}$`)
	gitBranchPat = regexp.MustCompile(`^[A-Za-z0-9._/\-]{1,250}$`)
	envKeyPat    = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,99}$`)
)

// Handler implements the EnvTie adapter protocol for Vercel preview hosting.
type Handler struct{}

type project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Link *struct {
		Type   string `json:"type"`
		RepoID int64  `json:"repoId"`
		Repo   string `json:"repo"`
		Org    string `json:"org"`
		GitJob *struct {
			Path string `json:"path"`
		} `json:"gitJob"`
	} `json:"link"`
}

type deployment struct {
	ID            string            `json:"id"`
	URL           string            `json:"url"`
	Name          string            `json:"name"`
	ReadyState    string            `json:"readyState"`
	ReadySubstate string            `json:"readySubstate"`
	Target        string            `json:"target"`
	Meta          map[string]string `json:"meta"`
	CreatedAt     int64             `json:"createdAt"`
}

type envVar struct {
	ID        string   `json:"id"`
	Key       string   `json:"key"`
	GitBranch string   `json:"gitBranch"`
	Target    []string `json:"target"`
}

type envList struct {
	Envs []envVar `json:"envs"`
}

func (Handler) Manifest() adapter.Manifest {
	return adapter.Manifest{
		Name: "vercel", Version: "1.0.0", Protocol: 1,
		Capabilities: []string{"read", "apply", "validate", "outputs", "destroy"},
		// GITHUB_HEAD_REF / GITHUB_SHA are Actions context. Without them the
		// runner would fall back to the EnvTie environment name (pr-N), which is
		// not a git branch and Vercel rejects with 400.
		CredentialEnv: []string{"VERCEL_TOKEN", "GITHUB_HEAD_REF", "GITHUB_SHA"},
		Inputs: []adapter.Input{
			{Name: "VERCEL_TOKEN", Kind: adapter.InputSecret, Required: true, Description: "Vercel access token with project deploy and env permissions"},
			{Name: "GITHUB_HEAD_REF", Kind: adapter.InputContext, Required: false, Description: "PR head branch from GitHub Actions (preferred git_branch when config omits it)"},
			{Name: "GITHUB_SHA", Kind: adapter.InputContext, Required: false, Description: "Commit SHA; used as gitSource.ref when set so the deploy matches the PR head"},
		},
		Outputs: []adapter.Field{
			{Name: "VERCEL_URL", Sensitivity: "PUBLIC"},
			{Name: "VERCEL_DEPLOYMENT_ID", Sensitivity: "PUBLIC"},
			{Name: "VERCEL_GIT_BRANCH", Sensitivity: "PUBLIC"},
		},
	}
}

func (Handler) Handle(ctx context.Context, r adapter.Request) (adapter.Response, error) {
	token := strings.TrimSpace(os.Getenv("VERCEL_TOKEN"))
	if token == "" {
		return adapter.Response{}, adapter.NewError("vercel_credential_missing")
	}
	auth := "Bearer " + token
	projectID := strings.TrimSpace(r.Config["project_id"])
	if !projectIDPat.MatchString(projectID) {
		return adapter.Response{}, adapter.NewError("vercel_project_required")
	}
	teamID := strings.TrimSpace(r.Config["team_id"])
	branch, err := resolveBranch(r)
	if err != nil {
		return adapter.Response{}, err
	}

	switch r.Hook {
	case "read":
		// Empty ExternalID stays Absent so apply's read-before-create does not
		// treat leftover branch deployments as an unmanaged collision. Destroy
		// cleans by branch even without a saved deployment id.
		if r.ExternalID == "" {
			return adapter.Response{Absent: true}, nil
		}
		dep, status, err := getDeployment(ctx, auth, teamID, r.ExternalID)
		if status == http.StatusNotFound {
			return adapter.Response{Absent: true}, nil
		}
		if err != nil {
			return adapter.Response{}, classify(status, err)
		}
		// Wrong-branch or already cleaned up: Absent so destroy verification converges.
		if !deploymentMatchesBranch(dep, branch) {
			return adapter.Response{Absent: true}, nil
		}
		return adapter.Response{ExternalID: dep.ID, Status: readyStatus(dep), Outputs: outputs(dep, branch)}, nil
	case "validate":
		if r.ExternalID == "" {
			return adapter.Response{Absent: true}, nil
		}
		dep, status, err := getDeployment(ctx, auth, teamID, r.ExternalID)
		if status == http.StatusNotFound {
			return adapter.Response{Absent: true}, nil
		}
		if err != nil {
			return adapter.Response{}, classify(status, err)
		}
		if !deploymentMatchesBranch(dep, branch) {
			return adapter.Response{}, adapter.NewError("vercel_deployment_unmanaged")
		}
		return adapter.Response{ExternalID: dep.ID, Status: readyStatus(dep), Outputs: outputs(dep, branch)}, nil
	case "apply":
		proj, status, err := getProject(ctx, auth, teamID, projectID)
		if err != nil {
			return adapter.Response{}, classify(status, err)
		}
		if proj.Link == nil || proj.Link.RepoID == 0 || strings.TrimSpace(proj.Link.Type) == "" {
			return adapter.Response{}, adapter.NewError("vercel_git_unlinked")
		}
		if err := upsertBranchEnv(ctx, auth, teamID, proj.ID, branch, envFromConfig(r.Config)); err != nil {
			return adapter.Response{}, err
		}
		dep, status, err := createGitDeployment(ctx, auth, teamID, proj, branch)
		if err != nil {
			return adapter.Response{}, classify(status, err)
		}
		dep, err = waitReady(ctx, auth, teamID, dep.ID)
		if err != nil {
			return adapter.Response{}, err
		}
		publicURL := resolvePublicURL(ctx, auth, teamID, dep, proj.Name, branch)
		// Drop the previous EnvTie-tracked deployment so re-plans do not pile up.
		if prev := strings.TrimSpace(r.ExternalID); prev != "" && prev != dep.ID {
			endpoint := withTeam(apiBase+"/v13/deployments/"+url.PathEscape(prev), teamID)
			status, delErr := httpclient.JSON(ctx, http.MethodDelete, endpoint, auth, nil, nil)
			if status != http.StatusNotFound && delErr != nil {
				return adapter.Response{}, classify(status, delErr)
			}
		}
		return adapter.Response{
			ExternalID: dep.ID,
			Status:     "created",
			Outputs:    outputsWithURL(dep, branch, publicURL),
		}, nil
	case "outputs":
		if r.ExternalID == "" {
			return adapter.Response{}, adapter.NewError("vercel_deployment_missing")
		}
		dep, status, err := getDeployment(ctx, auth, teamID, r.ExternalID)
		if err != nil {
			return adapter.Response{}, classify(status, err)
		}
		if !deploymentMatchesBranch(dep, branch) {
			return adapter.Response{}, adapter.NewError("vercel_deployment_unmanaged")
		}
		return adapter.Response{Outputs: outputs(dep, branch)}, nil
	case "destroy":
		proj, status, err := getProject(ctx, auth, teamID, projectID)
		if status == http.StatusNotFound {
			return adapter.Response{Absent: true}, nil
		}
		if err != nil {
			return adapter.Response{}, classify(status, err)
		}
		// Delete the tracked deployment first, then the rest of the branch.
		if r.ExternalID != "" {
			endpoint := withTeam(apiBase+"/v13/deployments/"+url.PathEscape(r.ExternalID), teamID)
			status, err := httpclient.JSON(ctx, http.MethodDelete, endpoint, auth, nil, nil)
			if status != http.StatusNotFound && err != nil {
				return adapter.Response{}, classify(status, err)
			}
		}
		if err := deleteBranchEnv(ctx, auth, teamID, proj.ID, branch); err != nil {
			return adapter.Response{}, err
		}
		if err := deleteBranchDeployments(ctx, auth, teamID, proj.ID, branch); err != nil {
			return adapter.Response{}, err
		}
		return adapter.Response{Absent: true}, nil
	default:
		return adapter.Response{}, adapter.NewError("vercel_invalid_request")
	}
}

func resolveBranch(r adapter.Request) (string, error) {
	branch := strings.TrimSpace(r.Config["git_branch"])
	if branch == "" {
		branch = strings.TrimSpace(os.Getenv("GITHUB_HEAD_REF"))
	}
	if branch == "" {
		branch = strings.TrimSpace(r.Environment)
	}
	branch = strings.ReplaceAll(branch, "{env}", r.Environment)
	if !gitBranchPat.MatchString(branch) {
		return "", adapter.NewError("vercel_branch_required")
	}
	return branch, nil
}

// deployRef is the gitSource.ref sent to Vercel. Prefer the Actions commit so
// the build matches the PR head; otherwise deploy the resolved branch tip.
func deployRef(branch string) string {
	if sha := strings.TrimSpace(os.Getenv("GITHUB_SHA")); sha != "" {
		return sha
	}
	return branch
}

func envFromConfig(cfg map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range cfg {
		name, ok := strings.CutPrefix(k, "env.")
		if !ok || !envKeyPat.MatchString(name) {
			continue
		}
		out[name] = v
	}
	return out
}

func outputs(dep deployment, branch string) map[string]string {
	return outputsWithURL(dep, branch, "")
}

func outputsWithURL(dep deployment, branch, publicURL string) map[string]string {
	u := strings.TrimSpace(publicURL)
	if u == "" {
		u = strings.TrimSpace(dep.URL)
	}
	if u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		u = "https://" + u
	}
	return map[string]string{
		"VERCEL_URL":           u,
		"VERCEL_DEPLOYMENT_ID": dep.ID,
		"VERCEL_GIT_BRANCH":    branch,
	}
}

// resolvePublicURL prefers the stable git-branch alias so Stripe webhooks and
// other callbacks survive redeploys of the same branch.
func resolvePublicURL(ctx context.Context, auth, teamID string, dep deployment, _projectName, _branch string) string {
	if alias := branchAlias(ctx, auth, teamID, dep.ID); alias != "" {
		return alias
	}
	return strings.TrimSpace(dep.URL)
}

func branchAlias(ctx context.Context, auth, teamID, deploymentID string) string {
	if deploymentID == "" {
		return ""
	}
	var body struct {
		Aliases []struct {
			Alias string `json:"alias"`
		} `json:"aliases"`
	}
	status, err := httpclient.JSON(ctx, http.MethodGet, withTeam(apiBase+"/v2/deployments/"+url.PathEscape(deploymentID)+"/aliases", teamID), auth, nil, &body)
	if err != nil || status >= 300 {
		return ""
	}
	for _, item := range body.Aliases {
		alias := strings.TrimSpace(strings.ToLower(item.Alias))
		if alias == "" {
			continue
		}
		if strings.Contains(alias, "-git-") {
			if !strings.HasPrefix(alias, "https://") {
				alias = "https://" + alias
			}
			return alias
		}
	}
	return ""
}

func readyStatus(dep deployment) string {
	if strings.EqualFold(dep.ReadyState, "READY") {
		return "ready"
	}
	return strings.ToLower(dep.ReadyState)
}

func deploymentMatchesBranch(dep deployment, branch string) bool {
	if meta := dep.Meta; meta != nil {
		for _, key := range []string{"githubCommitRef", "gitlabCommitRef", "bitbucketCommitRef", "gitBranch"} {
			if ref := strings.TrimSpace(meta[key]); ref != "" {
				return ref == branch
			}
		}
	}
	// ID-based lookups are authoritative when the provider omits commit meta.
	return true
}

func withTeam(rawURL, teamID string) string {
	if teamID == "" {
		return rawURL
	}
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return rawURL + sep + "teamId=" + url.QueryEscape(teamID)
}

func getProject(ctx context.Context, auth, teamID, idOrName string) (project, int, error) {
	var proj project
	status, err := httpclient.JSON(ctx, http.MethodGet, withTeam(apiBase+"/v9/projects/"+url.PathEscape(idOrName), teamID), auth, nil, &proj)
	return proj, status, err
}

func getDeployment(ctx context.Context, auth, teamID, id string) (deployment, int, error) {
	var dep deployment
	status, err := httpclient.JSON(ctx, http.MethodGet, withTeam(apiBase+"/v13/deployments/"+url.PathEscape(id), teamID), auth, nil, &dep)
	return dep, status, err
}

func createGitDeployment(ctx context.Context, auth, teamID string, proj project, branch string) (deployment, int, error) {
	linkType := strings.ToLower(strings.TrimSpace(proj.Link.Type))
	switch linkType {
	case "github", "gitlab", "bitbucket":
	default:
		return deployment{}, 0, adapter.NewError("vercel_git_unlinked")
	}
	// Omit target: the Deployments API only accepts "production", "staging", or
	// a custom environment id. Preview is the default when target is absent;
	// "preview" as a string is rejected with HTTP 400.
	body := map[string]any{
		"name":    proj.Name,
		"project": proj.ID,
		"gitSource": map[string]any{
			"type":   linkType,
			"repoId": proj.Link.RepoID,
			"ref":    deployRef(branch),
		},
	}
	var dep deployment
	status, err := httpclient.JSON(ctx, http.MethodPost, withTeam(apiBase+"/v13/deployments", teamID), auth, body, &dep)
	return dep, status, err
}

func waitReady(ctx context.Context, auth, teamID, id string) (deployment, error) {
	deadline := time.Now().Add(pollTimeout)
	for {
		dep, status, err := getDeployment(ctx, auth, teamID, id)
		if err != nil {
			return deployment{}, classify(status, err)
		}
		switch strings.ToUpper(dep.ReadyState) {
		case "READY":
			return dep, nil
		case "ERROR", "CANCELED", "BLOCKED":
			return deployment{}, adapter.NewError("vercel_deploy_failed")
		}
		if time.Now().After(deadline) {
			return deployment{}, adapter.NewError("vercel_deploy_timeout")
		}
		select {
		case <-ctx.Done():
			return deployment{}, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func upsertBranchEnv(ctx context.Context, auth, teamID, projectID, branch string, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	items := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		items = append(items, map[string]any{
			"key":       key,
			"value":     values[key],
			"type":      "encrypted",
			"target":    []string{"preview"},
			"gitBranch": branch,
		})
	}
	endpoint := withTeam(apiBase+"/v10/projects/"+url.PathEscape(projectID)+"/env?upsert=true", teamID)
	status, err := httpclient.JSON(ctx, http.MethodPost, endpoint, auth, items, nil)
	if err != nil {
		return classify(status, err)
	}
	return nil
}

func listBranchEnv(ctx context.Context, auth, teamID, projectID, branch string) ([]envVar, int, error) {
	q := url.Values{}
	q.Set("gitBranch", branch)
	if teamID != "" {
		q.Set("teamId", teamID)
	}
	var out envList
	status, err := httpclient.JSON(ctx, http.MethodGet, apiBase+"/v10/projects/"+url.PathEscape(projectID)+"/env?"+q.Encode(), auth, nil, &out)
	if err != nil {
		return nil, status, err
	}
	matched := make([]envVar, 0, len(out.Envs))
	for _, env := range out.Envs {
		if strings.TrimSpace(env.GitBranch) == branch {
			matched = append(matched, env)
		}
	}
	return matched, status, nil
}

func deleteBranchEnv(ctx context.Context, auth, teamID, projectID, branch string) error {
	for {
		envs, status, err := listBranchEnv(ctx, auth, teamID, projectID, branch)
		if status == http.StatusNotFound {
			return nil
		}
		if err != nil {
			return classify(status, err)
		}
		if len(envs) == 0 {
			return nil
		}
		deleted := 0
		for _, env := range envs {
			endpoint := withTeam(apiBase+"/v9/projects/"+url.PathEscape(projectID)+"/env/"+url.PathEscape(env.ID), teamID)
			status, err := httpclient.JSON(ctx, http.MethodDelete, endpoint, auth, nil, nil)
			if status == http.StatusNotFound {
				deleted++
				continue
			}
			if err != nil {
				return classify(status, err)
			}
			deleted++
		}
		if deleted == 0 || len(envs) < 100 {
			return nil
		}
	}
}

type deploymentPage struct {
	Deployments []deployment `json:"deployments"`
	Pagination  *struct {
		Count int   `json:"count"`
		Next  int64 `json:"next"`
	} `json:"pagination"`
}

func listAllBranchDeployments(ctx context.Context, auth, teamID, projectID, branch string) ([]deployment, int, error) {
	var all []deployment
	var until int64
	for {
		q := url.Values{}
		q.Set("projectId", projectID)
		q.Set("branch", branch)
		q.Set("limit", "100")
		if teamID != "" {
			q.Set("teamId", teamID)
		}
		if until > 0 {
			q.Set("until", strconv.FormatInt(until, 10))
		}
		var out deploymentPage
		status, err := httpclient.JSON(ctx, http.MethodGet, apiBase+"/v6/deployments?"+q.Encode(), auth, nil, &out)
		if err != nil {
			return nil, status, err
		}
		all = append(all, out.Deployments...)
		if out.Pagination == nil || out.Pagination.Next == 0 || len(out.Deployments) == 0 {
			return all, status, nil
		}
		until = out.Pagination.Next
	}
}

func deleteBranchDeployments(ctx context.Context, auth, teamID, projectID, branch string) error {
	for {
		deps, status, err := listAllBranchDeployments(ctx, auth, teamID, projectID, branch)
		if status == http.StatusNotFound {
			return nil
		}
		if err != nil {
			return classify(status, err)
		}
		if len(deps) == 0 {
			return nil
		}
		deleted := 0
		for _, dep := range deps {
			endpoint := withTeam(apiBase+"/v13/deployments/"+url.PathEscape(dep.ID), teamID)
			status, err := httpclient.JSON(ctx, http.MethodDelete, endpoint, auth, nil, nil)
			if status == http.StatusNotFound {
				deleted++
				continue
			}
			if err != nil {
				return classify(status, err)
			}
			deleted++
		}
		if deleted == 0 || len(deps) < 100 {
			return nil
		}
	}
}

func classify(status int, _ error) error {
	switch httpclient.StatusClass(status) {
	case "auth":
		return adapter.NewError("vercel_unauthorized")
	case "not_found":
		return adapter.NewError("vercel_not_found")
	case "quota":
		return adapter.NewError("vercel_quota")
	case "rate_limit":
		return adapter.NewError("vercel_rate_limited")
	case "invalid":
		return adapter.NewError("vercel_invalid_request")
	default:
		return adapter.NewError("vercel_unavailable")
	}
}
