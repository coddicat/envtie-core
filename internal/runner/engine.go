package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coddicat/envtie-core/core"
	signature "github.com/coddicat/envtie-core/plan"
	"github.com/coddicat/envtie-core/sdk/adapter"
)

type CallFunc func(context.Context, core.Component, adapter.Request) (adapter.Response, error)

func DefaultCall(ctx context.Context, c core.Component, req adapter.Request) (adapter.Response, error) {
	executable, err := executableForComponent(ctx, c)
	if err != nil {
		return adapter.Response{}, err
	}
	return executable.Call(ctx, req)
}

type SaveFunc func(*core.Environment) error
type DeliverFunc func(context.Context, string, string) error

type Engine struct {
	Call    CallFunc
	Save    SaveFunc
	Deliver DeliverFunc
}

func (e Engine) call(ctx context.Context, c core.Component, req adapter.Request) (adapter.Response, error) {
	if e.Call == nil {
		return DefaultCall(ctx, c, req)
	}
	return e.Call(ctx, c, req)
}

func (e Engine) save(env *core.Environment) error {
	if e.Save != nil {
		return e.Save(env)
	}
	return nil
}

func (e Engine) Apply(ctx context.Context, plan core.Plan, env *core.Environment) error {
	if time.Now().After(plan.ExpiresAt) {
		return errors.New("plan expired")
	}
	if env.Name != plan.Environment {
		return errors.New("environment mismatch")
	}
	if env.Resources == nil {
		env.Resources = map[string]core.Resource{}
	}
	available := map[string]bool{}
	creationOnly := map[string]bool{}
	for _, action := range plan.Actions {
		manifest, err := e.verifyAdapter(ctx, action.Component)
		if err != nil {
			var coded *adapter.CodedError
			if errors.As(err, &coded) {
				return adapter.WithPlace(action.Component.ID, "manifest", err)
			}
			return fmt.Errorf("%s: adapter contract invalid", action.Component.ID)
		}
		for _, field := range manifest.Outputs {
			key := action.Component.ID + "." + field.Name
			available[key] = true
			if field.Availability == adapter.AvailabilityCreate {
				creationOnly[key] = true
			}
		}
	}
	previouslyReady := env.State == "READY"
	for _, binding := range plan.Bindings {
		if !available[binding.From] {
			return fmt.Errorf("binding %s is not declared by an adapter", binding.From)
		}
	}
	env.State = "RUNNING"
	clearDiagnostics(env)
	appendLog(env, "info", "apply started")
	if err := e.save(env); err != nil {
		return err
	}
	outputs := map[string]string{}
	createdNow := map[string]bool{}
	for _, a := range plan.Actions {
		c := a.Component
		var createOutputs map[string]string
		prior, exists := env.Resources[c.ID]
		req := adapter.Request{Protocol: adapter.ProtocolVersion, Environment: env.Name, Component: c.ID, Config: copyConfig(c.Config), IdempotencyKey: plan.ID + "/" + c.ID}
		if exists {
			req.ExternalID = prior.ExternalID
		}
		if c.Ownership == core.Referenced {
			req.ExternalID = c.Config["id"]
		}
		if c.Adapter == "vercel@1" {
			if err := injectVercelEnvBindings(req.Config, plan.Bindings, outputs, creationOnly, previouslyReady, createdNow); err != nil {
				return e.fail(env, err)
			}
		}
		if req.ExternalID == "" && c.Ownership == core.Managed {
			// A deterministic provider name lets read-before-create recover from a lost POST response.
			// Preserve runner-injected keys (for example vercel env.*) already on req.Config.
			req.Config["name"] = managedConfigName(env.Name, c.ID)
			appendLog(env, "info", c.ID+" read")
			read, err := e.call(ctx, c, withHook(req, "read"))
			if err != nil {
				return e.fail(env, adapter.WithPlace(c.ID, "read", err))
			}
			if !read.Absent && read.ExternalID != "" {
				if !exists {
					return e.fail(env, fmt.Errorf("%s: provider name already exists without a local creation intent", c.ID))
				}
				req.ExternalID = read.ExternalID
			}
			if !exists {
				env.Resources[c.ID] = core.Resource{ComponentID: c.ID, Adapter: c.Adapter, Ownership: c.Ownership, Status: "creating"}
				if err := e.save(env); err != nil {
					return err
				}
			}
		}
		if req.ExternalID == "" {
			if c.Ownership == core.Referenced {
				return e.fail(env, fmt.Errorf("%s: referenced id missing", c.ID))
			}
			appendLog(env, "info", c.ID+" apply")
			resp, err := e.applyWithReconcile(ctx, c, req)
			if err != nil {
				return e.fail(env, err)
			}
			if resp.ExternalID == "" {
				return e.fail(env, fmt.Errorf("%s: adapter returned no id", c.ID))
			}
			req.ExternalID = resp.ExternalID
			createOutputs = resp.Outputs
			createdNow[c.ID] = true
			env.Resources[c.ID] = core.Resource{ComponentID: c.ID, Adapter: c.Adapter, Ownership: c.Ownership, ExternalID: resp.ExternalID, Status: "created"}
			if err := e.save(env); err != nil {
				return err
			}
		} else if c.Adapter == "vercel@1" && c.Ownership == core.Managed {
			// Reconcile branch env vars and trigger a fresh preview deployment on every plan.
			appendLog(env, "info", c.ID+" apply")
			resp, err := e.applyWithReconcile(ctx, c, req)
			if err != nil {
				return e.fail(env, err)
			}
			if resp.ExternalID == "" {
				return e.fail(env, fmt.Errorf("%s: adapter returned no id", c.ID))
			}
			req.ExternalID = resp.ExternalID
			createOutputs = resp.Outputs
			createdNow[c.ID] = true
			env.Resources[c.ID] = core.Resource{ComponentID: c.ID, Adapter: c.Adapter, Ownership: c.Ownership, ExternalID: resp.ExternalID, Status: "created"}
			if err := e.save(env); err != nil {
				return err
			}
		} else if !exists || prior.ExternalID == "" {
			env.Resources[c.ID] = core.Resource{ComponentID: c.ID, Adapter: c.Adapter, Ownership: c.Ownership, ExternalID: req.ExternalID, Status: "attached"}
			if err := e.save(env); err != nil {
				return err
			}
		}
		resp, err := e.retry(ctx, c, withHook(req, "validate"))
		if err != nil {
			return e.fail(env, err)
		}
		if resp.Absent {
			return e.fail(env, fmt.Errorf("%s: validation failed: resource absent", c.ID))
		}
		r := env.Resources[c.ID]
		r.Status = "ready"
		env.Resources[c.ID] = r
		appendLog(env, "info", c.ID+" ready")
		if err := e.save(env); err != nil {
			return err
		}
		resp, err = e.retry(ctx, c, withHook(req, "outputs"))
		if err != nil {
			return e.fail(env, err)
		}
		for k, v := range resp.Outputs {
			outputs[c.ID+"."+k] = v
		}
		for k, v := range createOutputs {
			if creationOnly[c.ID+"."+k] {
				outputs[c.ID+"."+k] = v
			}
		}
	}
	bindings := make([]core.Binding, 0, len(plan.Bindings))
	for _, b := range plan.Bindings {
		if _, ok := outputs[b.From]; !ok && creationOnly[b.From] {
			component := strings.SplitN(b.From, ".", 2)[0]
			if previouslyReady && !createdNow[component] {
				continue // already delivered when the resource was created
			}
			return e.fail(env, fmt.Errorf("%s is only revealed when the resource is created and was not delivered; destroy and recreate the environment", b.From))
		}
		bindings = append(bindings, b)
	}
	if err := e.bind(ctx, env.Name, bindings, outputs); err != nil {
		return e.fail(env, err)
	}
	appendLog(env, "info", "bindings written")
	env.State = "READY"
	clearDiagnostics(env)
	appendLog(env, "info", "apply completed")
	env.UpdatedAt = time.Now().UTC()
	return e.save(env)
}

func (e Engine) verifyAdapter(ctx context.Context, c core.Component) (*adapter.Manifest, error) {
	resp, err := e.call(ctx, c, adapter.Request{Protocol: adapter.ProtocolVersion, Hook: "manifest"})
	if err != nil {
		return nil, err
	}
	if resp.Manifest == nil || resp.Manifest.Protocol != adapter.ProtocolVersion {
		return nil, errors.New("protocol mismatch")
	}
	parts := strings.SplitN(c.Adapter, "@", 2)
	if len(parts) != 2 || resp.Manifest.Name != parts[0] || !strings.HasPrefix(resp.Manifest.Version, parts[1]+".") {
		return nil, errors.New("adapter identity mismatch")
	}
	needed := map[string]bool{"read": false, "validate": false, "outputs": false}
	if c.Ownership == core.Managed {
		needed["apply"] = false
		needed["destroy"] = false
	}
	for _, cap := range resp.Manifest.Capabilities {
		if _, ok := needed[cap]; ok {
			needed[cap] = true
		}
	}
	for _, present := range needed {
		if !present {
			return nil, errors.New("adapter capability missing")
		}
	}
	return resp.Manifest, nil
}

// retryBackoff is the base delay between provider retries.
var retryBackoff = time.Second

func (e Engine) applyWithReconcile(ctx context.Context, c core.Component, req adapter.Request) (adapter.Response, error) {
	var last error
	for i := 0; i < 3; i++ {
		resp, err := e.call(ctx, c, withHook(req, "apply"))
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return adapter.Response{}, ctx.Err()
		}
		last = adapter.WithPlace(c.ID, "apply", err)
		read, readErr := e.call(ctx, c, withHook(req, "read"))
		if readErr != nil {
			if ctx.Err() != nil {
				return adapter.Response{}, ctx.Err()
			}
			return adapter.Response{}, adapter.WithPlace(c.ID, "read", readErr)
		}
		if !read.Absent && read.ExternalID != "" {
			return read, nil
		}
		select {
		case <-ctx.Done():
			return adapter.Response{}, ctx.Err()
		case <-time.After(time.Duration(1<<i) * retryBackoff):
		}
	}
	return adapter.Response{}, last
}

func copyConfig(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func managedConfigName(environment, componentID string) string {
	return environment + "-" + componentID
}

func (e Engine) readManagedByName(ctx context.Context, environment string, c core.Component) (string, bool, error) {
	cfg := copyConfig(c.Config)
	cfg["name"] = managedConfigName(environment, c.ID)
	read, err := e.call(ctx, c, adapter.Request{
		Protocol:       adapter.ProtocolVersion,
		Hook:           "read",
		Environment:    environment,
		Component:      c.ID,
		Config:         cfg,
		IdempotencyKey: environment + "/destroy/" + c.ID,
	})
	if err != nil {
		return "", false, adapter.WithPlace(c.ID, "read", err)
	}
	if read.Absent {
		return "", true, nil
	}
	return read.ExternalID, false, nil
}

func withHook(r adapter.Request, hook string) adapter.Request { r.Hook = hook; return r }

func (e Engine) retry(ctx context.Context, c core.Component, r adapter.Request) (adapter.Response, error) {
	var err error
	for i := 0; i < 3; i++ {
		var resp adapter.Response
		resp, err = e.call(ctx, c, r)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return adapter.Response{}, ctx.Err()
		}
		err = adapter.WithPlace(c.ID, r.Hook, err)
		select {
		case <-ctx.Done():
			return adapter.Response{}, ctx.Err()
		case <-time.After(time.Duration(1<<i) * retryBackoff):
		}
	}
	return adapter.Response{}, err
}

func (e Engine) fail(env *core.Environment, err error) error {
	if len(env.Resources) > 0 {
		env.State = "DEGRADED"
	} else {
		env.State = "FAILED"
	}
	clearDiagnostics(env)
	env.LastError = "step failed; inspect runner logs"
	var failure *adapter.Failure
	if errors.As(err, &failure) {
		safe := adapter.NewFailure(failure.Code, failure.Component, failure.Hook)
		env.LastError = safe.Error()
		env.FailureCode = safe.Code
		env.FailureComponent = safe.Component
		env.FailurePhase = safe.Hook
	}
	appendLog(env, "error", env.LastError)
	env.UpdatedAt = time.Now().UTC()
	_ = e.save(env)
	return err
}

func clearDiagnostics(env *core.Environment) {
	env.LastError = ""
	env.FailureCode = ""
	env.FailureComponent = ""
	env.FailurePhase = ""
	env.Residuals = nil
}

// logMessage allows only constructed lifecycle text. Slashes are excluded so a
// URL or filesystem path cannot be checkpointed.
var logMessage = regexp.MustCompile(`^[a-z0-9_.:; -]{1,160}$`)

func appendLog(env *core.Environment, level, message string) {
	if env == nil || (level != "info" && level != "error") || !logMessage.MatchString(message) || len(env.Logs) >= 200 {
		return
	}
	env.Logs = append(env.Logs, core.LogLine{Seq: len(env.Logs) + 1, Level: level, Message: message})
}

// NoteFailure copies a sanitized adapter failure onto env when the engine
// returned before fail recorded one, such as a manifest mismatch. The hosted
// report can then persist the same fields checkpoints already carry.
func NoteFailure(env *core.Environment, err error) {
	if env == nil || err == nil || env.FailureCode != "" || env.LastError != "" {
		return
	}
	var failure *adapter.Failure
	if !errors.As(err, &failure) {
		// The control plane only accepts a terminal state in a failed report.
		if env.State == "PLANNED" || env.State == "RUNNING" {
			env.LastError = "step failed; inspect runner logs"
			appendLog(env, "error", env.LastError)
			if len(env.Resources) > 0 {
				env.State = "DEGRADED"
			} else {
				env.State = "FAILED"
			}
		}
		return
	}
	safe := adapter.NewFailure(failure.Code, failure.Component, failure.Hook)
	env.FailureCode = safe.Code
	env.FailureComponent = safe.Component
	env.FailurePhase = safe.Hook
	env.LastError = safe.Error()
	appendLog(env, "error", env.LastError)
	if env.State != "DEGRADED" && env.State != "FAILED" && env.State != "RESIDUAL" {
		if len(env.Resources) > 0 {
			env.State = "DEGRADED"
		} else {
			env.State = "FAILED"
		}
	}
}

func (e Engine) Destroy(ctx context.Context, b core.Blueprint, env *core.Environment, confirmation string) error {
	if env.Protected && confirmation != env.Name {
		return errors.New("protected environment: type its exact name to confirm")
	}
	order, err := b.Order()
	if err != nil {
		return err
	}
	env.State = "RUNNING"
	clearDiagnostics(env)
	appendLog(env, "info", "destroy started")
	if err := e.save(env); err != nil {
		return err
	}
	for i := len(order) - 1; i >= 0; i-- {
		c := order[i]
		r, ok := env.Resources[c.ID]
		if !ok {
			continue
		}
		if r.Ownership == core.Referenced {
			delete(env.Resources, c.ID)
			continue
		}
		if c.Ownership != core.Managed || r.Ownership != core.Managed || r.Adapter != c.Adapter {
			continue
		}
		if _, err := e.verifyAdapter(ctx, c); err != nil {
			continue
		}
		if r.ExternalID == "" && c.Adapter != "vercel@1" {
			// Creation can be checkpointed before the provider ID is saved.
			// Look up only the deterministic name from that create, then
			// destroy the returned ID. An empty ID or a caller-supplied
			// config name must not select an unrelated resource.
			// vercel@1 cleans by git branch and does not need a saved deployment id.
			resolved, absent, err := e.readManagedByName(ctx, env.Name, c)
			if err != nil || (resolved == "" && !absent) {
				continue
			}
			if absent {
				delete(env.Resources, c.ID)
				if err := e.save(env); err != nil {
					return err
				}
				continue
			}
			r.ExternalID = resolved
			env.Resources[c.ID] = r
			if err := e.save(env); err != nil {
				return err
			}
		}
		appendLog(env, "info", c.ID+" destroy")
		cfg := copyConfig(c.Config)
		cfg["name"] = managedConfigName(env.Name, c.ID)
		req := adapter.Request{Protocol: adapter.ProtocolVersion, Hook: "destroy", Environment: env.Name, Component: c.ID, Config: cfg, ExternalID: r.ExternalID, IdempotencyKey: env.Name + "/destroy/" + c.ID}
		if _, err := e.retry(ctx, c, req); err != nil {
			continue
		}
		// Always verify with read. Adapters that schedule async deletion (e.g. Sentry)
		// must report pending/in-progress deletion as Absent on read so this check passes.
		read, err := e.call(ctx, c, withHook(req, "read"))
		if err != nil || !read.Absent {
			continue
		}
		delete(env.Resources, c.ID)
		appendLog(env, "info", c.ID+" removed")
		if err := e.save(env); err != nil {
			return err
		}
	}
	// Account for all tracked resources, including ones omitted from the blueprint.
	// Omission never grants permission to mutate a resource or claim it is absent.
	var residual []string
	for id, r := range env.Resources {
		if r.Ownership == core.Referenced {
			delete(env.Resources, id)
		} else {
			residual = append(residual, id)
		}
	}
	sort.Strings(residual)
	env.Residuals = residual
	env.UpdatedAt = time.Now().UTC()
	if len(residual) > 0 {
		env.State = "RESIDUAL"
		env.Residuals = residual
		env.LastError = "some managed resources remain"
		env.FailureCode = "residual_resources"
		env.FailurePhase = "destroy"
		env.FailureComponent = ""
		appendLog(env, "error", env.LastError)
	} else {
		if err := removeEnvironmentDotenv(env.Name); err != nil {
			return err
		}
		env.State = "DESTROYED"
		clearDiagnostics(env)
		appendLog(env, "info", "destroy verified")
	}
	if err := e.save(env); err != nil {
		return err
	}
	if len(residual) > 0 {
		return fmt.Errorf("residual resources: %s", strings.Join(residual, ", "))
	}
	return nil
}

var dotenvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var environmentName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

func (e Engine) bind(ctx context.Context, environment string, bindings []core.Binding, outputs map[string]string) error {
	if !environmentName.MatchString(environment) {
		return errors.New("invalid environment name")
	}
	filePaths := map[string]string{}
	for _, b := range bindings {
		if _, ok := outputs[b.From]; !ok {
			return fmt.Errorf("binding %s: output absent", b.From)
		}
		parts := strings.SplitN(b.To, ".", 2)
		if len(parts) < 2 || parts[1] == "" {
			return errors.New("invalid binding")
		}
		switch parts[0] {
		case "env", "dotenv":
			if !dotenvKey.MatchString(parts[1]) {
				return errors.New("invalid binding key")
			}
		case "file":
			if filepath.IsAbs(parts[1]) {
				return errors.New("file binding path must be relative and contained")
			}
			path, err := safeBindingPath(parts[1])
			if err != nil {
				return err
			}
			filePaths[b.To] = path
		case "github":
			if _, _, ok := core.GitHubBinding(b.To); !ok || e.Deliver == nil {
				return errors.New("GitHub Environment delivery unavailable")
			}
		case "vercel":
			if _, ok := core.VercelEnvBinding(b.To); !ok {
				return errors.New("invalid Vercel env binding")
			}
			// Delivered into the vercel@1 apply request before deploy.
		default:
			return errors.New("unsupported binding")
		}
	}
	dotenv := map[string]string{}
	for _, b := range bindings {
		v := outputs[b.From]
		parts := strings.SplitN(b.To, ".", 2)
		switch parts[0] {
		case "env":
			if err := os.Setenv(parts[1], v); err != nil {
				return err
			}
		case "dotenv":
			dotenv[parts[1]] = v
		case "file":
			if err := atomicWrite(filePaths[b.To], []byte(v), 0600); err != nil {
				return err
			}
		case "github":
			if e.Deliver == nil {
				return errors.New("GitHub Environment delivery unavailable")
			}
			if err := e.Deliver(ctx, b.To, v); err != nil {
				return err
			}
		case "vercel":
			// Already applied during vercel@1.
		}
	}
	if len(dotenv) == 0 {
		return nil
	}
	return writeEnvironmentDotenv(environment, dotenv)
}

// bind keeps the local binding helper available to focused unit tests.
func bind(environment string, bindings []core.Binding, outputs map[string]string) error {
	return (Engine{}).bind(context.Background(), environment, bindings, outputs)
}

// injectVercelEnvBindings copies resolved outputs into config keys env.<NAME>
// for the vercel adapter. Creation-only outputs (for example MAILTRAP_SMTP_PASS)
// that were delivered on a prior READY apply are skipped so re-plans still work.
func injectVercelEnvBindings(cfg map[string]string, bindings []core.Binding, outputs map[string]string, creationOnly map[string]bool, previouslyReady bool, createdNow map[string]bool) error {
	if cfg == nil {
		return errors.New("vercel config missing")
	}
	for _, b := range bindings {
		name, ok := core.VercelEnvBinding(b.To)
		if !ok {
			continue
		}
		v, present := outputs[b.From]
		if !present {
			if creationOnly[b.From] {
				component := strings.SplitN(b.From, ".", 2)[0]
				if previouslyReady && !createdNow[component] {
					continue
				}
			}
			return fmt.Errorf("binding %s: output absent for Vercel env", b.From)
		}
		cfg["env."+name] = v
	}
	return nil
}

func environmentDotenvPath(environment string) (string, error) {
	if !environmentName.MatchString(environment) {
		return "", errors.New("invalid environment name")
	}
	return safeBindingPath(filepath.Join(".envtie", "bindings", environment+".env"))
}

func writeEnvironmentDotenv(environment string, values map[string]string) error {
	path, err := environmentDotenvPath(environment)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var body strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&body, "%s=%s\n", key, strconv.Quote(values[key]))
	}
	return atomicWrite(path, []byte(body.String()), 0600)
}

func removeEnvironmentDotenv(environment string) error {
	path, err := environmentDotenvPath(environment)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	path, err := safeBindingPath(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if _, err := safeBindingPath(path); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	closed := false
	remove := true
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		if remove {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	closed = true
	if err := renameReplace(tmpName, path); err != nil {
		return err
	}
	remove = false
	return os.Chmod(path, mode)
}

func safeBindingPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("file binding path must be relative and contained")
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) && (clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator))) {
		return "", errors.New("file binding path must be relative and contained")
	}
	root, err := os.Getwd()
	if err != nil {
		return "", err
	}
	target := clean
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("file binding path must be relative and contained")
	}
	current := root
	parts := strings.Split(relative, string(filepath.Separator))
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			break
		}
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("file binding path must not contain symlinks")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return "", errors.New("file binding parent must be a directory")
		}
		if i == len(parts)-1 && !info.Mode().IsRegular() {
			return "", errors.New("file binding destination must be a regular file")
		}
	}
	return target, nil
}

func renameReplace(oldpath, newpath string) error {
	err := os.Rename(oldpath, newpath)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	if rmErr := os.Remove(newpath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return err
	}
	return os.Rename(oldpath, newpath)
}

func SanitizeOutputs(outputs map[string]string) map[string]string {
	safe := map[string]string{}
	for k, v := range outputs {
		safe[k] = signature.Fingerprint(v)
	}
	return safe
}
