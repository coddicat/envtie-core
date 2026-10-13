package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"time"
)

const ProtocolVersion = 1

// AvailabilityCreate marks an output the provider reveals only in the create
// response (for example a Stripe webhook signing secret). Adapters return it
// from the apply hook; the runner binds it immediately and skips re-binding it
// on later applies because the provider cannot return it again.
const AvailabilityCreate = "create"

type Field struct {
	Name         string `json:"name"`
	Sensitivity  string `json:"sensitivity"`
	Availability string `json:"availability,omitempty"`
}

type Manifest struct {
	Name          string   `json:"name"`
	Version       string   `json:"version"`
	Protocol      int      `json:"protocol"`
	Capabilities  []string `json:"capabilities"`
	Outputs       []Field  `json:"outputs"`
	CredentialEnv []string `json:"credential_env"`
}

type Request struct {
	Protocol       int               `json:"protocol"`
	Hook           string            `json:"hook"`
	Environment    string            `json:"environment"`
	Component      string            `json:"component"`
	Config         map[string]string `json:"config"`
	ExternalID     string            `json:"external_id,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
}

type Response struct {
	Manifest   *Manifest         `json:"manifest,omitempty"`
	ExternalID string            `json:"external_id,omitempty"`
	Status     string            `json:"status,omitempty"`
	Outputs    map[string]string `json:"outputs,omitempty"`
	Absent     bool              `json:"absent,omitempty"`
	ErrorCode  string            `json:"error_code,omitempty"`
}

type Handler interface {
	Manifest() Manifest
	Handle(context.Context, Request) (Response, error)
}

func Serve(h Handler) {
	var req Request
	dec := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		write(Response{ErrorCode: "invalid_request"})
		return
	}
	if req.Protocol != ProtocolVersion {
		write(Response{ErrorCode: "protocol_mismatch"})
		return
	}
	if req.Hook == "manifest" {
		m := h.Manifest()
		write(Response{Manifest: &m})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	resp, err := h.Handle(ctx, req)
	if err != nil {
		resp = errorResponse(err)
	}
	write(resp)
}

func errorResponse(err error) Response {
	resp := Response{ErrorCode: "adapter_failed"}
	var coded *CodedError
	if errors.As(err, &coded) && errorCode.MatchString(coded.Code) {
		resp.ErrorCode = coded.Code
	}
	return resp
}

var errorCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var componentName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

var hookName = map[string]bool{
	"manifest": true,
	"read":     true,
	"apply":    true,
	"validate": true,
	"outputs":  true,
	"destroy":  true,
}

// CodedError lets a handler report a stable, non-sensitive error code (for
// example "stripe_endpoint_limit") instead of the generic "adapter_failed".
type CodedError struct{ Code string }

func (e *CodedError) Error() string { return e.Code }

func NewError(code string) error { return &CodedError{Code: code} }

// Failure is a stable adapter error safe to return, log, and persist.
// Component and Hook locate the failed step. The text never includes a
// provider body, URL, or secret; those are dropped even if a caller wrapped them.
type Failure struct {
	Code      string
	Component string
	Hook      string
}

func NewFailure(code, component, hook string) *Failure {
	if !errorCode.MatchString(code) {
		code = "adapter_failed"
	}
	if !componentName.MatchString(component) {
		component = ""
	}
	if !hookName[hook] {
		hook = ""
	}
	return &Failure{Code: code, Component: component, Hook: hook}
}

// WithPlace attaches the component and hook to the stable code inside err.
// Errors without a stable code become adapter_failed so provider details cannot surface.
func WithPlace(component, hook string, err error) error {
	code := "adapter_failed"
	var existing *Failure
	if errors.As(err, &existing) && errorCode.MatchString(existing.Code) {
		code = existing.Code
		if component == "" {
			component = existing.Component
		}
		if hook == "" {
			hook = existing.Hook
		}
	} else {
		var coded *CodedError
		if errors.As(err, &coded) && errorCode.MatchString(coded.Code) {
			code = coded.Code
		}
	}
	return NewFailure(code, component, hook)
}

func (e *Failure) Error() string {
	if e == nil {
		return "adapter_failed"
	}
	code, component, hook := e.Code, e.Component, e.Hook
	if !errorCode.MatchString(code) {
		code = "adapter_failed"
	}
	if !componentName.MatchString(component) {
		component = ""
	}
	if !hookName[hook] {
		hook = ""
	}
	switch {
	case component != "" && hook != "":
		return component + " " + hook + ": " + code
	case component != "":
		return component + ": " + code
	case hook != "":
		return hook + ": " + code
	default:
		return code
	}
}

func (e *Failure) Unwrap() error {
	code := "adapter_failed"
	if e != nil && errorCode.MatchString(e.Code) {
		code = e.Code
	}
	return &CodedError{Code: code}
}

func write(resp Response) { _ = json.NewEncoder(os.Stdout).Encode(resp) }

type Executable struct {
	Path   string
	SHA256 string
}

func (a Executable) Call(ctx context.Context, req Request) (Response, error) {
	if a.Path == "" {
		return Response{}, errors.New("adapter executable missing")
	}
	if err := VerifyChecksum(a.Path, a.SHA256); err != nil {
		return Response{}, err
	}
	env := baseEnvironment()
	if req.Hook != "manifest" {
		manifest, err := a.run(ctx, Request{Protocol: ProtocolVersion, Hook: "manifest"}, env)
		if err != nil {
			return Response{}, err
		}
		if manifest.Manifest == nil || manifest.Manifest.Protocol != ProtocolVersion {
			return Response{}, errors.New("invalid adapter manifest")
		}
		for _, name := range manifest.Manifest.CredentialEnv {
			if !environmentName.MatchString(name) {
				return Response{}, errors.New("invalid adapter environment declaration")
			}
			if value, ok := os.LookupEnv(name); ok {
				env = append(env, name+"="+value)
			}
		}
	}
	return a.run(ctx, req, env)
}

var environmentName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func baseEnvironment() []string {
	result := []string{}
	for _, name := range []string{"PATH", "SystemRoot", "WINDIR", "HOME", "USERPROFILE", "TMP", "TEMP"} {
		if value, ok := os.LookupEnv(name); ok {
			result = append(result, name+"="+value)
		}
	}
	return result
}

func (a Executable) run(ctx context.Context, req Request, env []string) (Response, error) {
	input, _ := json.Marshal(req)
	c, cancel := context.WithTimeout(ctx, 95*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, a.Path)
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(input)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard // Never capture provider responses or secret-shaped diagnostics.
	if err := cmd.Run(); err != nil {
		return Response{}, fmt.Errorf("adapter process failed: %T", err)
	}
	if out.Len() > 1<<20 {
		return Response{}, errors.New("adapter response too large")
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		return Response{}, errors.New("adapter returned invalid JSON")
	}
	if resp.ErrorCode != "" {
		return Response{}, NewFailure(resp.ErrorCode, req.Component, req.Hook)
	}
	return resp, nil
}
