package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coddicat/envtie-core/core"
	"github.com/coddicat/envtie-core/githubsetup"
	"github.com/coddicat/envtie-core/internal/envx"
	"github.com/coddicat/envtie-core/internal/runner"
	plansig "github.com/coddicat/envtie-core/plan"
	"github.com/coddicat/envtie-core/sdk/adapter"
	"golang.org/x/crypto/nacl/box"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "envtie:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: envtie preview apply|destroy|close <environment> [--run] | preview sweep [--run] | preview status <operation-id> | github setup | runner execute [--once] | blueprint validate [file] | plan | apply | status | outputs | destroy | version")
	}
	switch args[0] {
	case "version", "--version":
		fmt.Println(version)
		return nil
	case "blueprint":
		if len(args) < 2 || args[1] != "validate" {
			return errors.New("usage: blueprint validate [file]")
		}
		path := "envtie.yaml"
		if len(args) > 2 {
			path = args[2]
		}
		b, err := readBlueprint(path)
		if err != nil {
			return err
		}
		fmt.Printf("valid blueprint %s (%d components)\n", b.Name, len(b.Components))
		return nil
	case "plan":
		return plan(args[1:])
	case "apply":
		return apply(args[1:])
	case "retry":
		return apply(args[1:])
	case "status":
		return status(args[1:])
	case "outputs":
		return outputs(args[1:])
	case "destroy":
		return destroy(args[1:])
	case "preview":
		return preview(args[1:])
	case "github":
		return githubSetup(args[1:])
	case "adapter":
		if len(args) != 3 || args[1] != "test" {
			return errors.New("usage: adapter test <executable>")
		}
		return testAdapter(args[2])
	case "runner":
		if len(args) < 2 || args[1] != "execute" {
			return errors.New("usage: runner execute [--once]")
		}
		f := flag.NewFlagSet("runner execute", flag.ContinueOnError)
		once := f.Bool("once", false, "run the jobs that are waiting, then exit (for CI and scheduled jobs)")
		if err := f.Parse(args[2:]); err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		cfg, err := runnerConfigFromEnv()
		if err != nil {
			return err
		}
		cfg.once = *once
		err = executeRunner(ctx, cfg)
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "runner: stopped")
			return nil
		}
		return err
	case "login":
		return errors.New("hosted OIDC login is configured through the web dashboard")
	case "keygen":
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		if err := ensureDir(); err != nil {
			return err
		}
		privatePath := filepath.Join(stateDir(), "controlplane-private.key")
		publicPath := filepath.Join(stateDir(), "controlplane-public.key")
		if err := os.WriteFile(privatePath, []byte(base64.StdEncoding.EncodeToString(priv)), 0600); err != nil {
			return err
		}
		if err := os.WriteFile(publicPath, []byte(base64.StdEncoding.EncodeToString(pub)), 0644); err != nil {
			return err
		}
		fmt.Printf("Ed25519 keypair written to %s and %s\n", privatePath, publicPath)
		return nil
	default:
		return errors.New("unknown command")
	}
}

func readBlueprint(path string) (core.Blueprint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return core.Blueprint{}, err
	}
	return core.ParseBlueprint(data)
}

type previewOperation struct {
	OperationID string `json:"operation_id"`
	Status      string `json:"status"`
	Failure     *struct {
		Code      string `json:"code"`
		Component string `json:"component"`
		Phase     string `json:"phase"`
		Message   string `json:"message"`
	} `json:"failure,omitempty"`
}

func preview(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: preview apply|destroy|close <environment> [--run] | preview sweep [--run] | preview status <operation-id>")
	}
	switch args[0] {
	case "execute":
		return previewExecute(args[1:])
	case "report":
		return previewReport(args[1:])
	case "apply", "destroy", "close":
		return previewLifecycle(args[0], args[1:])
	case "sweep":
		return previewSweep(args[1:])
	case "status":
		f := flag.NewFlagSet("preview status", flag.ContinueOnError)
		watch := f.Bool("watch", false, "wait for the operation to finish")
		timeout := f.Duration("timeout", 15*time.Minute, "maximum time to wait")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 1 {
			return errors.New("usage: preview status <operation-id> [--watch] [--timeout 15m]")
		}
		deadline := time.Now().Add(*timeout)
		for {
			var result previewOperation
			if err := automationRequest(http.MethodGet, "/v1/custom-ci/operations/"+url.PathEscape(f.Arg(0)), &result); err != nil {
				return err
			}
			if !*watch || result.Status == "completed" {
				fmt.Println(result.Status)
				return nil
			}
			if result.Status == "failed" || result.Status == "cancelled" || result.Status == "expired" {
				if result.Failure != nil {
					return fmt.Errorf("operation %s: %s", result.Status, result.Failure.Message)
				}
				return fmt.Errorf("operation %s", result.Status)
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timed out waiting for operation %s", f.Arg(0))
			}
			time.Sleep(3 * time.Second)
		}
	default:
		return errors.New("usage: preview apply|destroy|close <environment> [--run] | preview sweep [--run] | preview status <operation-id>")
	}
}

func previewLifecycle(action string, args []string) error {
	name, runIt, err := previewActionArgs(action, args)
	if err != nil {
		return err
	}
	path := "/v1/custom-ci/environments/" + url.PathEscape(name) + "/" + action
	if runIt {
		path += "?execute=1"
	}
	var result struct {
		previewOperation
		ExpiresAt time.Time `json:"expires_at"`
	}
	var report any
	if pr := workflowPullRequest(name); pr != nil {
		report = map[string]any{"pull_request": pr}
	}
	if err := automationSend(http.MethodPost, path, report, &result); err != nil {
		return err
	}
	if result.Status == "grace" {
		fmt.Println("grace until " + result.ExpiresAt.UTC().Format(time.RFC3339))
		return nil
	}
	if result.OperationID == "" {
		fmt.Println(result.Status)
		return nil
	}
	if !runIt {
		fmt.Println(result.OperationID)
		return nil
	}
	fmt.Println(result.OperationID)
	return runAutomationOperation(result.OperationID)
}

// previewActionArgs accepts the flag on either side of the environment name.
// The generated workflow passes it last, and Go's flag parser would otherwise
// treat that as a second argument.
func previewActionArgs(action string, args []string) (string, bool, error) {
	name := ""
	runIt := false
	for _, arg := range args {
		if arg == "--run" {
			runIt = true
			continue
		}
		if strings.HasPrefix(arg, "-") || name != "" {
			return "", false, fmt.Errorf("usage: preview %s <environment> [--run]", action)
		}
		name = arg
	}
	if name == "" {
		return "", false, fmt.Errorf("usage: preview %s <environment> [--run]", action)
	}
	return name, runIt, nil
}

// workflowPullRequest reads non-sensitive pull-request metadata from the
// GitHub Actions event file when it matches the pr-<number> environment.
func workflowPullRequest(environment string) map[string]any {
	path := os.Getenv("GITHUB_EVENT_PATH")
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var event struct {
		PullRequest *struct {
			Number  int    `json:"number"`
			Title   string `json:"title"`
			State   string `json:"state"`
			HTMLURL string `json:"html_url"`
			User    struct {
				Login string `json:"login"`
			} `json:"user"`
			Head struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
	}
	if json.Unmarshal(raw, &event) != nil || event.PullRequest == nil {
		return nil
	}
	pr := event.PullRequest
	if environment != "pr-"+strconv.Itoa(pr.Number) {
		return nil
	}
	return map[string]any{
		"repository": os.Getenv("GITHUB_REPOSITORY"),
		"number":     pr.Number,
		"title":      pr.Title,
		"branch":     pr.Head.Ref,
		"sha":        pr.Head.SHA,
		"author":     pr.User.Login,
		"state":      pr.State,
		"url":        pr.HTMLURL,
	}
}

func previewSweep(args []string) error {
	f := flag.NewFlagSet("preview sweep", flag.ContinueOnError)
	runIt := f.Bool("run", false, "destroy each expired environment in this process")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("usage: preview sweep [--run]")
	}
	var result struct {
		Environments []string `json:"environments"`
	}
	if err := automationRequest(http.MethodPost, "/v1/custom-ci/sweep", &result); err != nil {
		return err
	}
	if !*runIt {
		for _, name := range result.Environments {
			fmt.Println(name)
		}
		return nil
	}
	for _, name := range result.Environments {
		if err := previewLifecycle("destroy", []string{"--run", name}); err != nil {
			return err
		}
	}
	return nil
}

func githubSetup(args []string) error {
	if len(args) == 0 || args[0] != "setup" {
		return errors.New("usage: envtie github setup [--name <app-name>]")
	}
	f := flag.NewFlagSet("github setup", flag.ContinueOnError)
	suffix := make([]byte, 3)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	name := f.String("name", fmt.Sprintf("envtie-%x", suffix), "GitHub App name, unique across GitHub (at most 34 characters)")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	redirect := "https://envtie.dev/github/setup"
	if app := strings.TrimRight(os.Getenv("ENVTIE_APP_URL"), "/"); app != "" {
		redirect = app + "/github/setup"
	}
	manifest, err := githubsetup.AppManifest(*name, redirect)
	if err != nil {
		return err
	}
	fmt.Println(manifest)
	fmt.Fprintln(os.Stderr, "Submit this manifest at https://github.com/settings/apps/new. GitHub gives the private key to you; Envtie does not keep a copy.")
	return nil
}

func runAutomationOperation(id string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	base := strings.TrimRight(envx.Get("API_URL"), "/")
	token := automationToken()
	if !secureAPIURL(base) || token == "" {
		return errors.New("HTTPS ENVTIE_API_URL and ENVTIE_API_KEY are required")
	}
	cfg := runnerConfig{
		base: base, token: token, client: &http.Client{Timeout: 30 * time.Second},
		jobsRoot: "/v1/custom-ci/operations", exactOperation: id,
	}
	job, err := cfg.lease(ctx)
	if err != nil {
		return err
	}
	if job.OperationID != id {
		return errors.New("leased a different operation")
	}
	return cfg.runJob(ctx, job)
}

func automationToken() string {
	if token := envx.Get("CI_TOKEN"); token != "" {
		return token
	}
	return os.Getenv("ENVTIE_API_KEY")
}

type dispatchExchange struct {
	Session     string `json:"session"`
	OperationID string `json:"operation_id"`
	PublicKey   string `json:"public_key"`
	Status      string `json:"status"`
}

func previewExecute(args []string) error {
	return previewExecuteCall(args, nil)
}

func previewExecuteCall(args []string, call runner.CallFunc) error {
	f := flag.NewFlagSet("preview execute", flag.ContinueOnError)
	dispatch := f.String("dispatch", "", "EnvTie.dev dispatch id")
	timeout := f.Duration("timeout", 15*time.Minute, "maximum time to wait")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *dispatch == "" || f.NArg() != 0 || strings.ContainsAny(*dispatch, " \t\r\n/\\") {
		return errors.New("usage: preview execute --dispatch <id> [--timeout 15m]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	oidcToken, err := requestActionsIDToken(ctx, "envtie")
	if err != nil {
		return err
	}
	base := strings.TrimRight(envx.Get("API_URL"), "/")
	if !secureAPIURL(base) {
		return errors.New("HTTPS ENVTIE_API_URL (plain HTTP only for localhost) is required")
	}
	exchanged, err := exchangeDispatch(ctx, base, oidcToken, *dispatch)
	if err != nil {
		return err
	}
	if exchanged.OperationID == "" && exchanged.Status == "destroyed" {
		fmt.Println("destroyed")
		return nil
	}
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(exchanged.PublicKey))
	if err != nil || len(pub) != ed25519.PublicKeySize || !strings.HasPrefix(exchanged.Session, "epci_") || exchanged.OperationID == "" {
		return errors.New("control plane exchange rejected")
	}
	cfg := runnerConfig{
		base: base, token: exchanged.Session, pub: ed25519.PublicKey(pub),
		client: &http.Client{Timeout: 30 * time.Second}, call: call, idle: 3 * time.Second,
		jobsRoot: "/v1/ci/jobs", exactOperation: exchanged.OperationID,
	}
	fmt.Println(exchanged.OperationID)
	job, err := cfg.lease(ctx)
	if errors.Is(err, errNoJob) {
		return errors.New("operation was not waiting")
	}
	if err != nil {
		return err
	}
	if job.OperationID != exchanged.OperationID {
		return errors.New("leased a different operation")
	}
	runErr := cfg.runJob(ctx, job)
	watchErr := cfg.watchOperation(ctx, exchanged.OperationID)
	if watchErr != nil {
		return watchErr
	}
	return runErr
}

func previewReport(args []string) error {
	f := flag.NewFlagSet("preview report", flag.ContinueOnError)
	dispatch := f.String("dispatch", "", "EnvTie.dev dispatch id")
	status := f.String("status", "", "test result: success or failure")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *dispatch == "" || (*status != "success" && *status != "failure") || f.NArg() != 0 ||
		strings.ContainsAny(*dispatch, " \t\r\n/\\") {
		return errors.New("usage: preview report --dispatch <id> --status success|failure")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	oidcToken, err := requestActionsIDToken(ctx, "envtie")
	if err != nil {
		return err
	}
	base := strings.TrimRight(envx.Get("API_URL"), "/")
	if !secureAPIURL(base) {
		return errors.New("HTTPS ENVTIE_API_URL (plain HTTP only for localhost) is required")
	}
	payload, err := json.Marshal(map[string]string{"dispatch_id": *dispatch, "status": *status})
	if err != nil {
		return errors.New("could not encode preview report")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/github/actions/report", strings.NewReader(string(payload)))
	if err != nil {
		return errors.New("could not report preview result")
	}
	req.Header.Set("Authorization", "Bearer "+oidcToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return errors.New("could not report preview result")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("preview report returned HTTP %d", resp.StatusCode)
	}
	fmt.Println("reported " + *status)
	return nil
}

func requestActionsIDToken(ctx context.Context, audience string) (string, error) {
	rawURL := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")
	requestToken := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if rawURL == "" || requestToken == "" {
		return "", errors.New("ACTIONS_ID_TOKEN_REQUEST_URL and ACTIONS_ID_TOKEN_REQUEST_TOKEN are required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("ACTIONS_ID_TOKEN_REQUEST_URL is invalid")
	}
	host := parsed.Hostname()
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		return "", errors.New("ACTIONS_ID_TOKEN_REQUEST_URL is invalid")
	}
	query := parsed.Query()
	query.Set("audience", audience)
	parsed.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", errors.New("GitHub identity token request failed")
	}
	req.Header.Set("Authorization", "bearer "+requestToken)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", errors.New("GitHub identity token request failed")
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil || resp.StatusCode != http.StatusOK {
		return "", errors.New("GitHub identity token request failed")
	}
	var out struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(payload, &out) != nil || out.Value == "" || len(out.Value) > 20000 {
		return "", errors.New("GitHub identity token request failed")
	}
	return out.Value, nil
}

func exchangeDispatch(ctx context.Context, base, oidcToken, dispatchID string) (dispatchExchange, error) {
	var result dispatchExchange
	encoded, err := json.Marshal(map[string]string{"dispatch_id": dispatchID})
	if err != nil {
		return result, errors.New("control plane exchange failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/github/actions/exchange", strings.NewReader(string(encoded)))
	if err != nil {
		return result, errors.New("control plane exchange failed")
	}
	req.Header.Set("Authorization", "Bearer "+oidcToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return result, errors.New("control plane exchange failed")
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return result, errors.New("control plane exchange failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiError struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &apiError)
		if apiError.Error != "" && !strings.Contains(apiError.Error, oidcToken) {
			if apiError.Message != "" && !strings.Contains(apiError.Message, oidcToken) {
				return result, fmt.Errorf("%s: %s", apiError.Error, apiError.Message)
			}
			return result, errors.New(apiError.Error)
		}
		return result, fmt.Errorf("control plane returned HTTP %d", resp.StatusCode)
	}
	if json.Unmarshal(payload, &result) != nil {
		return result, errors.New("control plane exchange rejected")
	}
	return result, nil
}

func (cfg runnerConfig) watchOperation(ctx context.Context, id string) error {
	path := "/v1/ci/operations/" + url.PathEscape(id)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("timed out waiting for operation %s", id)
		}
		var result previewOperation
		if err := cfg.getJSON(ctx, path, &result); err != nil {
			return err
		}
		switch result.Status {
		case "completed":
			fmt.Println(result.Status)
			return nil
		case "failed", "cancelled", "expired":
			if result.Failure != nil && result.Failure.Message != "" {
				return fmt.Errorf("operation %s: %s", result.Status, result.Failure.Message)
			}
			return fmt.Errorf("operation %s", result.Status)
		}
		pause(ctx, 3*time.Second)
	}
}

func (cfg runnerConfig) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.base+path, nil)
	if err != nil {
		return errors.New("control plane request failed")
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	req.Header.Set("Accept", "application/json")
	resp, err := cfg.client.Do(req)
	if err != nil {
		return errors.New("control plane unavailable")
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return errors.New("control plane unavailable")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &leaseStatusError{code: resp.StatusCode}
	}
	if json.Unmarshal(payload, out) != nil {
		return errors.New("invalid operation status")
	}
	return nil
}

func automationRequest(method, path string, out any) error {
	return automationSend(method, path, nil, out)
}

func automationSend(method, path string, in, out any) error {
	base := strings.TrimRight(envx.Get("API_URL"), "/")
	token := automationToken()
	if base == "" || token == "" {
		return errors.New("ENVTIE_API_URL and ENVTIE_API_KEY are required")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1")) {
		return errors.New("ENVTIE_API_URL must be HTTPS (plain HTTP is allowed only for localhost)")
	}
	var reader io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiError struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &apiError)
		if apiError.Message != "" {
			return fmt.Errorf("%s: %s", apiError.Error, apiError.Message)
		}
		if apiError.Error != "" {
			return errors.New(apiError.Error)
		}
		return fmt.Errorf("control plane returned HTTP %d", resp.StatusCode)
	}
	if out != nil && len(payload) > 0 {
		return json.Unmarshal(payload, out)
	}
	return nil
}

func stateDir() string           { return ".envtie" }
func ensureDir() error           { return os.MkdirAll(stateDir(), 0700) }
func envPath(name string) string { return filepath.Join(stateDir(), "env-"+name+".json") }

func saveJSON(path string, value any) error {
	if err := ensureDir(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
func loadJSON(path string, value any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}
func loadEnv(name string) (core.Environment, error) {
	var env core.Environment
	err := loadJSON(envPath(name), &env)
	return env, err
}

func keypair() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	path := filepath.Join(stateDir(), "local-signing-key.json")
	var stored struct {
		Public  []byte `json:"public"`
		Private []byte `json:"private"`
	}
	if err := loadJSON(path, &stored); err == nil {
		return ed25519.PublicKey(stored.Public), ed25519.PrivateKey(stored.Private), nil
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	stored.Public, stored.Private = pub, priv
	if err := saveJSON(path, stored); err != nil {
		return nil, nil, err
	}
	return pub, priv, nil
}

func plan(args []string) error {
	f := flag.NewFlagSet("plan", flag.ContinueOnError)
	name := f.String("env", "", "environment name")
	blueprint := f.String("blueprint", "envtie.yaml", "blueprint path")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("--env required")
	}
	b, err := readBlueprint(*blueprint)
	if err != nil {
		return err
	}
	var existing *core.Environment
	if e, err := loadEnv(*name); err == nil {
		existing = &e
	} else if !os.IsNotExist(err) {
		return err
	}
	p, err := core.NewPlan(b, *name, existing, time.Now())
	if err != nil {
		return err
	}
	_, priv, err := keypair()
	if err != nil {
		return err
	}
	if err := plansig.Sign(&p, "local", priv); err != nil {
		return err
	}
	if err := saveJSON(filepath.Join(stateDir(), "plan-"+p.ID+".json"), p); err != nil {
		return err
	}
	if err := saveJSON(filepath.Join(stateDir(), "blueprint-"+p.ID+".json"), b); err != nil {
		return err
	}
	fmt.Printf("Plan %s for %s (expires %s)\n", p.ID, p.Environment, p.ExpiresAt.Format(time.RFC3339))
	for _, a := range p.Actions {
		fmt.Printf("  %-8s %-20s %s\n", strings.ToUpper(a.Kind), a.Component.ID, a.Reason)
	}
	fmt.Printf("Apply: envtie apply %s\n", p.ID)
	return nil
}

func apply(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: apply <plan-id>")
	}
	var p core.Plan
	if err := loadJSON(filepath.Join(stateDir(), "plan-"+args[0]+".json"), &p); err != nil {
		return err
	}
	if p.ID != args[0] {
		return errors.New("plan id mismatch")
	}
	pub, _, err := keypair()
	if err != nil {
		return err
	}
	if err := plansig.Verify(p, "local", pub, time.Now()); err != nil {
		return err
	}
	var b core.Blueprint
	if err := loadJSON(filepath.Join(stateDir(), "blueprint-"+p.ID+".json"), &b); err != nil {
		return err
	}
	if err := b.Validate(); err != nil {
		return err
	}
	if b.Digest() != p.BlueprintDigest {
		return errors.New("stored blueprint does not match signed plan")
	}
	var env core.Environment
	if e, err := loadEnv(p.Environment); err == nil {
		env = e
	} else if os.IsNotExist(err) {
		env = core.Environment{Name: p.Environment, State: "PLANNED", Resources: map[string]core.Resource{}}
	} else {
		return err
	}
	env.Protected = p.Protect
	env.ExpiresAt = p.ResourceExpiresAt
	// Keep the authorized adapter configuration for partial cleanup and destroy.
	if err := saveJSON(filepath.Join(stateDir(), "blueprint-env-"+env.Name+".json"), b); err != nil {
		return err
	}
	engine := runner.Engine{Save: func(e *core.Environment) error { return saveJSON(envPath(e.Name), e) }}
	if err := engine.Apply(context.Background(), p, &env); err != nil {
		return err
	}
	fmt.Printf("%s READY (%d resources)\n", env.Name, len(env.Resources))
	return nil
}

func status(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: status <env> [--watch]")
	}
	watch := len(args) > 1 && args[1] == "--watch"
	for {
		env, err := loadEnv(args[0])
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(env, "", "  ")
		fmt.Println(string(b))
		if !watch || env.State == "READY" || env.State == "DESTROYED" || env.State == "FAILED" || env.State == "RESIDUAL" {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
}

func outputs(args []string) error {
	f := flag.NewFlagSet("outputs", flag.ContinueOnError)
	blueprint := f.String("blueprint", "envtie.yaml", "blueprint path")
	reveal := f.Bool("reveal", false, "reveal secret outputs locally (disabled in CI)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if err := allowOutputReveal(*reveal); err != nil {
		return err
	}
	if f.NArg() != 1 {
		return errors.New("usage: outputs [--blueprint file] [--reveal] <env>")
	}
	env, err := loadEnv(f.Arg(0))
	if err != nil {
		return err
	}
	if env.State != "READY" {
		return errors.New("environment not ready")
	}
	b, err := readBlueprint(*blueprint)
	if err != nil {
		return err
	}
	result, err := outputValues(context.Background(), b, env, *reveal, runner.DefaultCall)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

func allowOutputReveal(reveal bool) error {
	if reveal && (os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "" || os.Getenv("GITHUB_RUN_ID") != "") {
		return errors.New("secret output reveal is disabled in CI")
	}
	return nil
}

func outputValues(ctx context.Context, b core.Blueprint, env core.Environment, reveal bool, call runner.CallFunc) (map[string]string, error) {
	result := map[string]string{}
	for _, c := range b.Components {
		r, ok := env.Resources[c.ID]
		if !ok {
			continue
		}
		if r.Adapter != c.Adapter || r.Ownership != c.Ownership {
			return nil, errors.New("output blueprint differs from tracked resource")
		}
		manifest, err := call(ctx, c, adapter.Request{Protocol: 1, Hook: "manifest"})
		if err != nil || manifest.Manifest == nil || manifest.Manifest.Protocol != adapter.ProtocolVersion {
			return nil, errors.New("output manifest unavailable")
		}
		identity := strings.SplitN(c.Adapter, "@", 2)
		if len(identity) != 2 || manifest.Manifest.Name != identity[0] || !strings.HasPrefix(manifest.Manifest.Version, identity[1]+".") {
			return nil, errors.New("output adapter identity mismatch")
		}
		public := map[string]bool{}
		for _, field := range manifest.Manifest.Outputs {
			public[field.Name] = field.Sensitivity == "PUBLIC"
		}
		req := adapter.Request{Protocol: 1, Hook: "outputs", Environment: env.Name, Component: c.ID, Config: c.Config, ExternalID: r.ExternalID}
		resp, err := call(ctx, c, req)
		if err != nil {
			return nil, fmt.Errorf("%s: output retrieval failed", c.ID)
		}
		for key, value := range resp.Outputs {
			if !reveal && !public[key] {
				value = "[REDACTED]"
			}
			result[c.ID+"."+key] = value
		}
	}
	return result, nil
}

func destroy(args []string) error {
	f := flag.NewFlagSet("destroy", flag.ContinueOnError)
	confirm := f.String("confirm", "", "exact environment name for protected environments")
	blueprint := f.String("blueprint", "envtie.yaml", "blueprint path")
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name = args[0]
		args = args[1:]
	}
	if err := f.Parse(args); err != nil {
		return err
	}
	if name == "" && f.NArg() == 1 {
		name = f.Arg(0)
	} else if f.NArg() != 0 {
		return errors.New("usage: destroy <env> [--confirm <env>]")
	}
	if name == "" {
		return errors.New("usage: destroy <env> [--confirm <env>]")
	}
	env, err := loadEnv(name)
	if err != nil {
		return err
	}
	var b core.Blueprint
	if err := loadJSON(filepath.Join(stateDir(), "blueprint-env-"+env.Name+".json"), &b); err != nil {
		return errors.New("approved blueprint snapshot unavailable; apply a new plan before destroy")
	}
	if err := b.Validate(); err != nil {
		return errors.New("approved blueprint snapshot invalid")
	}
	var explicitBlueprint bool
	f.Visit(func(flag *flag.Flag) { explicitBlueprint = explicitBlueprint || flag.Name == "blueprint" })
	if explicitBlueprint {
		requested, err := readBlueprint(*blueprint)
		if err != nil || requested.Digest() != b.Digest() {
			return errors.New("destroy blueprint differs from approved snapshot")
		}
	}
	engine := runner.Engine{Save: func(e *core.Environment) error { return saveJSON(envPath(e.Name), e) }}
	if err := engine.Destroy(context.Background(), b, &env, *confirm); err != nil {
		return err
	}
	fmt.Printf("%s DESTROYED\n", env.Name)
	return nil
}

func testAdapter(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	h := sha256File(f)
	f.Close()
	if h == "" {
		return errors.New("checksum failed")
	}
	a := adapter.Executable{Path: path, SHA256: h}
	r, err := a.Call(context.Background(), adapter.Request{Protocol: 1, Hook: "manifest"})
	if err != nil {
		return err
	}
	if r.Manifest == nil || r.Manifest.Protocol != 1 {
		return errors.New("invalid manifest")
	}
	b, _ := json.MarshalIndent(r.Manifest, "", "  ")
	fmt.Println(string(b))
	fmt.Println("sha256:", h)
	return nil
}

func sha256File(r io.Reader) string {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return ""
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

type runnerConfig struct {
	base, token    string
	pub            ed25519.PublicKey
	client         *http.Client
	once           bool
	call           runner.CallFunc
	idle           time.Duration
	jobsRoot       string
	exactOperation string
}

func (cfg runnerConfig) jobsRootPath() string {
	if cfg.jobsRoot != "" {
		return strings.TrimRight(cfg.jobsRoot, "/")
	}
	return "/v1/runner/jobs"
}

// maxOnceJobs bounds a --once run so a misbehaving control plane cannot keep a CI job alive forever.
const maxOnceJobs = 25

func runnerConfigFromEnv() (runnerConfig, error) {
	base := strings.TrimRight(envx.Get("API_URL"), "/")
	token := envx.Get("RUNNER_TOKEN")
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(envx.Get("PUBLIC_KEY")))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return runnerConfig{}, errors.New("ENVTIE_PUBLIC_KEY must be base64 Ed25519 public key")
	}
	if !secureAPIURL(base) || token == "" {
		return runnerConfig{}, errors.New("HTTPS ENVTIE_API_URL (plain HTTP only for localhost) and ENVTIE_RUNNER_TOKEN required")
	}
	return runnerConfig{base: base, token: token, pub: ed25519.PublicKey(pub), client: &http.Client{Timeout: 30 * time.Second}, idle: 5 * time.Second}, nil
}

type runnerJob struct {
	OperationID string           `json:"operation_id"`
	WorkspaceID string           `json:"workspace_id"`
	Kind        string           `json:"kind"`
	Plan        core.Plan        `json:"plan"`
	Environment core.Environment `json:"environment"`
}

var errNoJob = errors.New("no job")

// executeRunner polls for approved jobs. In watch mode it runs until ctx is
// cancelled; with once it drains the waiting jobs and returns an error if the
// control plane was unusable or any job failed, so CI marks the step red.
func executeRunner(ctx context.Context, cfg runnerConfig) error {
	if cfg.once {
		fmt.Fprintln(os.Stderr, "runner: checking", cfg.base, "for approved jobs")
	} else {
		fmt.Fprintln(os.Stderr, "runner: polling", cfg.base, "for jobs (Ctrl+C to stop)")
	}
	unauthorized := false
	done, failed := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if cfg.once && done >= maxOnceJobs {
			fmt.Fprintf(os.Stderr, "runner: stopping after %d jobs; the rest run next time\n", done)
			break
		}
		job, err := cfg.lease(ctx)
		if errors.Is(err, errNoJob) {
			if cfg.once {
				break
			}
			pause(ctx, cfg.idle)
			continue
		}
		var status *leaseStatusError
		if errors.As(err, &status) && status.code == 401 {
			if cfg.once {
				return errors.New("runner token rejected (revoked, expired, or issued by another control plane)")
			}
			if !unauthorized {
				fmt.Fprintln(os.Stderr, "runner: token rejected (revoked, expired, or wrong control plane); retrying")
				unauthorized = true
			}
			pause(ctx, 2*cfg.idle)
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if cfg.once {
				return fmt.Errorf("control plane unavailable: %w", err)
			}
			pause(ctx, cfg.idle)
			continue
		}
		unauthorized = false
		done++
		if err := cfg.runJob(ctx, job); err != nil {
			failed++
			fmt.Fprintln(os.Stderr, "runner: operation failed:", err)
		} else {
			fmt.Fprintf(os.Stderr, "runner: %s %s completed\n", job.Kind, job.Environment.Name)
		}
	}
	fmt.Fprintf(os.Stderr, "runner: %d job(s) run, %d failed\n", done, failed)
	if failed > 0 {
		return fmt.Errorf("%d of %d job(s) failed", failed, done)
	}
	return nil
}

type leaseStatusError struct{ code int }

func (e *leaseStatusError) Error() string {
	return fmt.Sprintf("control plane returned HTTP %d", e.code)
}

func (cfg *runnerConfig) lease(ctx context.Context) (runnerJob, error) {
	var job runnerJob
	path := cfg.jobsRootPath()
	if cfg.exactOperation != "" {
		path += "/" + url.PathEscape(cfg.exactOperation) + "/lease"
	}
	req, err := http.NewRequestWithContext(ctx, "POST", cfg.base+path, nil)
	if err != nil {
		return job, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	resp, err := cfg.client.Do(req)
	if err != nil {
		return job, errors.New("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 {
		return job, errNoJob
	}
	if resp.StatusCode != 200 {
		return job, &leaseStatusError{code: resp.StatusCode}
	}
	var payload struct {
		runnerJob
		PublicKey string `json:"public_key"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return job, errors.New("invalid job payload")
	}
	if len(cfg.pub) == 0 && payload.PublicKey != "" {
		raw, err := base64.StdEncoding.DecodeString(payload.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return job, errors.New("control plane signing key rejected")
		}
		cfg.pub = raw
	}
	return payload.runnerJob, nil
}

func (cfg runnerConfig) runJob(ctx context.Context, job runnerJob) error {
	if plansig.Verify(job.Plan, job.WorkspaceID, cfg.pub, time.Now()) != nil {
		return fmt.Errorf("rejected operation %s: plan signature invalid or expired", job.OperationID)
	}
	fmt.Fprintf(os.Stderr, "runner: %s %s (operation %s)\n", job.Kind, job.Environment.Name, job.OperationID)
	jobCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	engine := runner.Engine{Call: cfg.call, Save: func(e *core.Environment) error {
		status, err := cfg.post(jobCtx, cfg.jobsRootPath()+"/"+url.PathEscape(job.OperationID)+"/checkpoint", map[string]any{"environment": e})
		if err != nil {
			return errors.New("checkpoint unavailable")
		}
		if status != 204 {
			return errors.New("checkpoint rejected")
		}
		return nil
	}}
	engine.Deliver = func(ctx context.Context, destination, value string) error {
		return cfg.deliverGitHubEnvironment(ctx, job.Environment.Name, destination, value)
	}
	var err error
	switch job.Kind {
	case "apply":
		err = engine.Apply(jobCtx, job.Plan, &job.Environment)
	case "destroy":
		b := core.Blueprint{Name: "destroy", Policy: core.Policy{Protect: job.Plan.Protect}}
		for _, a := range job.Plan.Actions {
			b.Components = append(b.Components, a.Component)
		}
		err = engine.Destroy(jobCtx, b, &job.Environment, job.Environment.Name)
	default:
		err = errors.New("unknown job kind")
	}
	runner.NoteFailure(&job.Environment, err)
	report := struct {
		Environment core.Environment `json:"environment"`
		Success     bool             `json:"success"`
	}{Environment: job.Environment, Success: err == nil}
	status, postErr := cfg.post(ctx, cfg.jobsRootPath()+"/"+url.PathEscape(job.OperationID)+"/report", report)
	if err != nil {
		if postErr != nil || status >= 300 {
			fmt.Fprintf(os.Stderr, "runner: the control plane did not accept the failure report (HTTP %d); the operation stays queued until its lease expires\n", status)
		}
		return err
	}
	if postErr != nil || status >= 300 {
		return errors.New("result could not be reported to the control plane")
	}
	return nil
}

func (cfg runnerConfig) post(ctx context.Context, path string, payload any) (int, error) {
	return cfg.send(ctx, http.MethodPost, path, payload)
}

func (cfg runnerConfig) put(ctx context.Context, path string, payload any) (int, error) {
	return cfg.send(ctx, http.MethodPut, path, payload)
}

func (cfg runnerConfig) send(ctx context.Context, method, path string, payload any) (int, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.base+path, strings.NewReader(string(data)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cfg.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

var githubAPI = "https://api.github.com"

func (cfg runnerConfig) deliverGitHubEnvironment(ctx context.Context, environment, destination, value string) error {
	kind, name, ok := core.GitHubBinding(destination)
	if !ok {
		return errors.New("unsupported GitHub Environment binding")
	}
	if os.Getenv("ENVTIE_FORK") == "true" {
		return errors.New("github_fork_secrets_unavailable")
	}
	token := os.Getenv("GITHUB_TOKEN")
	repository := os.Getenv("GITHUB_REPOSITORY")
	owner, repo, found := strings.Cut(repository, "/")
	if token == "" || !found || owner == "" || repo == "" || environment == "" {
		return errors.New("GitHub Environment delivery unavailable")
	}
	base := githubAPI + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/environments/" + url.PathEscape(environment)
	if err := requireExistingGitHubEnvironment(ctx, token, base); err != nil {
		return err
	}
	switch kind {
	case "secret":
		var key struct {
			KeyID string `json:"key_id"`
			Key   string `json:"key"`
		}
		if err := githubGET(ctx, token, base+"/secrets/public-key", &key); err != nil {
			return errors.New("GitHub Environment key unavailable")
		}
		raw, err := base64.StdEncoding.DecodeString(key.Key)
		if err != nil || len(raw) != 32 || key.KeyID == "" {
			return errors.New("GitHub Environment key rejected")
		}
		var recipient [32]byte
		copy(recipient[:], raw)
		sealed, err := box.SealAnonymous(nil, []byte(value), &recipient, rand.Reader)
		if err != nil {
			return errors.New("GitHub Environment secret encryption failed")
		}
		if err := githubSend(ctx, http.MethodPut, base+"/secrets/"+url.PathEscape(name), token, map[string]string{
			"key_id": key.KeyID, "encrypted_value": base64.StdEncoding.EncodeToString(sealed),
		}); err != nil {
			return errors.New("GitHub Environment secret delivery failed")
		}
	case "variable":
		if err := githubSend(ctx, http.MethodPost, base+"/variables", token, map[string]string{"name": name, "value": value}); err != nil {
			if err := githubSend(ctx, http.MethodPatch, base+"/variables/"+url.PathEscape(name), token, map[string]string{"name": name, "value": value}); err != nil {
				return errors.New("GitHub Environment variable delivery failed")
			}
		}
	case "url":
		if err := githubDeploymentURL(ctx, token, owner, repo, environment, value); err != nil {
			return errors.New("GitHub deployment URL delivery failed")
		}
	default:
		return errors.New("unsupported GitHub Environment binding")
	}
	return nil
}

// requireExistingGitHubEnvironment checks the environment the GitHub adapter
// created. Delivery writes secrets and variables there and does not create
// another environment.
func requireExistingGitHubEnvironment(ctx context.Context, token, base string) error {
	if err := githubGET(ctx, token, base, &map[string]any{}); err != nil {
		return errors.New("GitHub Environment delivery unavailable")
	}
	return requireManagedEnvironment(ctx, token, base)
}

func requireManagedEnvironment(ctx context.Context, token, base string) error {
	var page struct {
		Variables []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"variables"`
	}
	if err := githubGET(ctx, token, base+"/variables?per_page=100", &page); err != nil {
		return errors.New("GitHub Environment delivery unavailable")
	}
	for _, variable := range page.Variables {
		if variable.Name == "ENVTIE_MANAGED" && variable.Value == "true" {
			return nil
		}
	}
	return errors.New("github_environment_unmanaged")
}

func githubDeploymentURL(ctx context.Context, token, owner, repo, environment, value string) error {
	var deployments []struct {
		ID int64 `json:"id"`
	}
	path := githubAPI + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/deployments?environment=" + url.QueryEscape(environment) + "&per_page=1"
	if sha := os.Getenv("GITHUB_SHA"); sha != "" {
		path += "&sha=" + url.QueryEscape(sha)
	}
	if err := githubGET(ctx, token, path, &deployments); err != nil || len(deployments) == 0 || deployments[0].ID <= 0 {
		return errors.New("deployment missing")
	}
	statusPath := fmt.Sprintf("%s/repos/%s/%s/deployments/%d/statuses", githubAPI, url.PathEscape(owner), url.PathEscape(repo), deployments[0].ID)
	return githubSend(ctx, http.MethodPost, statusPath, token, map[string]any{
		"state": "success", "environment": environment, "environment_url": value, "description": "Envtie preview",
	})
}

func githubGET(ctx context.Context, token, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("github HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func githubSend(ctx context.Context, method, endpoint, token string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("github HTTP %d", resp.StatusCode)
	}
	return nil
}

func (cfg runnerConfig) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	req.Header.Set("Accept", "application/json")
	resp, err := cfg.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &leaseStatusError{code: resp.StatusCode}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func pause(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func secureAPIURL(base string) bool {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	host := u.Hostname()
	return u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")
}
