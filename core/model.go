package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Ownership string

const (
	Managed    Ownership = "managed"
	Referenced Ownership = "referenced"
)

type Blueprint struct {
	Name       string      `yaml:"name" json:"name"`
	Components []Component `yaml:"components" json:"components"`
	Bindings   []Binding   `yaml:"bindings" json:"bindings"`
	Policy     Policy      `yaml:"policy" json:"policy"`
}

type Component struct {
	ID         string                     `yaml:"id" json:"id"`
	Adapter    string                     `yaml:"adapter" json:"adapter"`
	Executable string                     `yaml:"executable" json:"executable,omitempty"`
	SHA256     string                     `yaml:"sha256" json:"sha256,omitempty"`
	Artifacts  map[string]AdapterArtifact `yaml:"-" json:"artifacts,omitempty"`
	Source     *AdapterSource             `yaml:"source,omitempty" json:"source,omitempty"`
	Ownership  Ownership                  `yaml:"ownership" json:"ownership"`
	DependsOn  []string                   `yaml:"depends_on" json:"depends_on,omitempty"`
	Config     map[string]string          `yaml:"config" json:"config,omitempty"`
}

// AdapterSource identifies a custom publisher registry. The runner accepts its
// signed release metadata only when Publisher has a configured trust key.
type AdapterSource struct {
	Kind      string `yaml:"kind" json:"kind"`
	Registry  string `yaml:"registry" json:"registry"`
	Publisher string `yaml:"publisher" json:"publisher"`
	Version   string `yaml:"version" json:"version"`
}

// AdapterArtifact is immutable release metadata embedded in a signed plan.
// Blueprints never author this field; the control plane resolves it from the
// official catalog when a blueprint version is saved.
type AdapterArtifact struct {
	Release string `json:"release"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size,omitempty"`
}

type Binding struct {
	From string `yaml:"from" json:"from"`
	To   string `yaml:"to" json:"to"`
}

type Policy struct {
	ExpiresIn          string `yaml:"expires_in" json:"expires_in,omitempty"`
	Protect            bool   `yaml:"protect" json:"protect"`
	DestroyGracePeriod string `yaml:"destroy_grace_period,omitempty" json:"destroy_grace_period,omitempty"`
}

type Action struct {
	Component Component `json:"component"`
	Kind      string    `json:"kind"`
	Reason    string    `json:"reason"`
}

type Plan struct {
	ID                string     `json:"id"`
	Environment       string     `json:"environment"`
	BlueprintDigest   string     `json:"blueprint_digest"`
	CreatedAt         time.Time  `json:"created_at"`
	ExpiresAt         time.Time  `json:"expires_at"`
	ResourceExpiresAt *time.Time `json:"resource_expires_at,omitempty"`
	Actions           []Action   `json:"actions"`
	Bindings          []Binding  `json:"bindings"`
	Protect           bool       `json:"protect"`
	Signature         string     `json:"signature,omitempty"`
}

type Resource struct {
	ComponentID string    `json:"component_id"`
	Adapter     string    `json:"adapter"`
	Ownership   Ownership `json:"ownership"`
	ExternalID  string    `json:"external_id"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Status      string    `json:"status"`
	ProviderURL string    `json:"provider_url,omitempty"`
}

// LogLine is a runner-authored lifecycle line. Messages are constructed by the
// engine from component ids and stable codes. They are not adapter stderr or
// provider response bodies.
type LogLine struct {
	Seq     int    `json:"seq"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

type Environment struct {
	Name             string              `json:"name"`
	State            string              `json:"state"`
	Protected        bool                `json:"protected"`
	ExpiresAt        *time.Time          `json:"expires_at,omitempty"`
	Resources        map[string]Resource `json:"resources"`
	Residuals        []string            `json:"residuals,omitempty"`
	LastError        string              `json:"last_error,omitempty"`
	FailureCode      string              `json:"failure_code,omitempty"`
	FailureComponent string              `json:"failure_component,omitempty"`
	FailurePhase     string              `json:"failure_phase,omitempty"`
	Logs             []LogLine           `json:"logs,omitempty"`
	UpdatedAt        time.Time           `json:"updated_at"`
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
var adapterName = regexp.MustCompile(`^[a-z][a-z0-9-]*@[0-9]+$`)
var repositoryName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var bindingName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,99}$`)

// GitHubBinding parses a GitHub Environment destination. Secret and variable
// names intentionally follow GitHub's portable uppercase convention.
func GitHubBinding(destination string) (kind, name string, ok bool) {
	if destination == "github.environment.url" {
		return "url", "", true
	}
	for _, candidate := range []string{"secret", "variable"} {
		prefix := "github.environment." + candidate + "."
		if strings.HasPrefix(destination, prefix) {
			name = strings.TrimPrefix(destination, prefix)
			if bindingName.MatchString(name) {
				return candidate, name, true
			}
			return "", "", false
		}
	}
	return "", "", false
}

// VercelEnvBinding parses a branch-scoped Vercel project env destination.
// The runner injects these into the vercel@1 apply request before deploy.
func VercelEnvBinding(destination string) (name string, ok bool) {
	const prefix = "vercel.env."
	if !strings.HasPrefix(destination, prefix) {
		return "", false
	}
	name = strings.TrimPrefix(destination, prefix)
	if bindingName.MatchString(name) {
		return name, true
	}
	return "", false
}

func githubBindingsWithoutProvider(b Blueprint) bool {
	hasProvider := false
	for _, c := range b.Components {
		if c.Adapter == "github@1" {
			hasProvider = true
			break
		}
	}
	if hasProvider {
		return false
	}
	for _, binding := range b.Bindings {
		if _, _, github := GitHubBinding(binding.To); github {
			return true
		}
	}
	return false
}

func vercelBindingsWithoutProvider(b Blueprint) bool {
	hasProvider := false
	for _, c := range b.Components {
		if c.Adapter == "vercel@1" {
			hasProvider = true
			break
		}
	}
	if hasProvider {
		return false
	}
	for _, binding := range b.Bindings {
		if _, ok := VercelEnvBinding(binding.To); ok {
			return true
		}
	}
	return false
}

// vercelEnvSourcesOrdered ensures each vercel.env.* source is ordered relative
// to vercel@1: either vercel depends on the source (inject before first deploy),
// or the source depends on vercel (runner refreshes env after the source applies,
// for example Stripe webhook secrets that need VERCEL_URL first).
func vercelEnvSourcesOrdered(b Blueprint, seen map[string]Component) error {
	reachable := map[string]map[string]bool{}
	var walk func(string, map[string]bool)
	walk = func(id string, into map[string]bool) {
		c, ok := seen[id]
		if !ok {
			return
		}
		for _, dep := range c.DependsOn {
			if into[dep] {
				continue
			}
			into[dep] = true
			walk(dep, into)
		}
	}
	for id := range seen {
		into := map[string]bool{}
		walk(id, into)
		reachable[id] = into
	}
	for _, binding := range b.Bindings {
		if _, ok := VercelEnvBinding(binding.To); !ok {
			continue
		}
		source := strings.SplitN(binding.From, ".", 2)[0]
		ok := false
		for _, c := range b.Components {
			if c.Adapter != "vercel@1" {
				continue
			}
			if reachable[c.ID][source] || reachable[source][c.ID] {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("vercel env binding %q requires an order between vercel@1 and %q", binding.To, source)
		}
	}
	return nil
}

// HasGitHubProvider reports whether the blueprint declares the GitHub
// environment provider.
func HasGitHubProvider(b Blueprint) bool {
	for _, c := range b.Components {
		if c.Adapter == "github@1" && c.Ownership == Managed {
			return true
		}
	}
	return false
}

// IsLocalBinding reports whether a destination is delivered only inside the
// process that runs the signed plan.
func IsLocalBinding(destination string) bool {
	return strings.HasPrefix(destination, "env.") ||
		strings.HasPrefix(destination, "file.") ||
		strings.HasPrefix(destination, "dotenv.")
}

func ParseBlueprint(data []byte) (Blueprint, error) {
	var b Blueprint
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&b); err != nil {
		return b, fmt.Errorf("blueprint YAML: %w", err)
	}
	return b, b.Validate()
}

func (b Blueprint) Validate() error {
	if !identifier.MatchString(b.Name) {
		return errors.New("invalid blueprint name")
	}
	if len(b.Components) == 0 {
		return errors.New("blueprint needs at least one component")
	}
	seen := map[string]Component{}
	for _, c := range b.Components {
		if !identifier.MatchString(c.ID) {
			return fmt.Errorf("invalid component id %q", c.ID)
		}
		if _, ok := seen[c.ID]; ok {
			return fmt.Errorf("duplicate component %q", c.ID)
		}
		if !adapterName.MatchString(c.Adapter) {
			return fmt.Errorf("adapter %q must be pinned to a major version", c.Adapter)
		}
		official := c.Adapter == "neon@1" || c.Adapter == "upstash@1" || c.Adapter == "stripe@1" || c.Adapter == "github@1" || c.Adapter == "sentry@1" || c.Adapter == "mailtrap@1" || c.Adapter == "vercel@1"
		if official && c.Source != nil {
			return fmt.Errorf("%s: official adapters cannot override their source", c.ID)
		}
		if !official && c.Source == nil && (c.Executable == "" || len(c.SHA256) != 64) {
			return fmt.Errorf("%s: custom adapters require a registry source or local executable and SHA-256 pin", c.ID)
		}
		if c.Source != nil {
			if c.Source.Kind != "registry" || c.Source.Registry == "" || !identifier.MatchString(c.Source.Publisher) || c.Source.Version == "" {
				return fmt.Errorf("%s: invalid custom adapter registry source", c.ID)
			}
			if c.Executable != "" || c.SHA256 != "" {
				return fmt.Errorf("%s: registry adapters cannot set local executable fields", c.ID)
			}
		}
		if (c.Executable == "") != (c.SHA256 == "") {
			return fmt.Errorf("%s: executable and SHA-256 pin must be set together", c.ID)
		}
		if c.Ownership != Managed && c.Ownership != Referenced {
			return fmt.Errorf("%s: ownership must be managed or referenced", c.ID)
		}
		if c.Ownership == Referenced && c.Config["id"] == "" {
			return fmt.Errorf("%s: referenced component needs config.id", c.ID)
		}
		for key, value := range c.Config {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "password") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "api_key") || strings.Contains(value, "://") {
				return fmt.Errorf("%s: config contains a secret-shaped field; use runner environment", c.ID)
			}
		}
		if c.Adapter == "github@1" && c.Ownership == Managed && !repositoryName.MatchString(c.Config["repository"]) {
			return fmt.Errorf("%s: repository must be owner/name", c.ID)
		}
		seen[c.ID] = c
	}
	for _, c := range b.Components {
		for _, dep := range c.DependsOn {
			if dep == c.ID {
				return fmt.Errorf("%s depends on itself", c.ID)
			}
			if _, ok := seen[dep]; !ok {
				return fmt.Errorf("%s: unknown dependency %s", c.ID, dep)
			}
		}
	}
	if _, err := b.Order(); err != nil {
		return err
	}
	destinations := map[string]bool{}
	for _, binding := range b.Bindings {
		parts := strings.SplitN(binding.From, ".", 2)
		if len(parts) != 2 || parts[1] == "" {
			return fmt.Errorf("invalid binding source %q", binding.From)
		}
		if _, ok := seen[parts[0]]; !ok {
			return fmt.Errorf("binding source component %q absent", parts[0])
		}
		_, _, github := GitHubBinding(binding.To)
		_, vercel := VercelEnvBinding(binding.To)
		if !IsLocalBinding(binding.To) && !github && !vercel {
			return fmt.Errorf("unsupported binding destination %q", binding.To)
		}
		if destinations[binding.To] {
			return fmt.Errorf("duplicate binding destination %q", binding.To)
		}
		destinations[binding.To] = true
	}
	if githubBindingsWithoutProvider(b) {
		return errors.New("GitHub environment bindings require a github@1 component")
	}
	if vercelBindingsWithoutProvider(b) {
		return errors.New("Vercel env bindings require a vercel@1 component")
	}
	if err := vercelEnvSourcesOrdered(b, seen); err != nil {
		return err
	}
	secrets := 0
	for _, binding := range b.Bindings {
		kind, _, ok := GitHubBinding(binding.To)
		if ok && kind == "secret" {
			secrets++
		}
	}
	if secrets > 100 {
		return errors.New("GitHub allows at most 100 secrets on an environment")
	}
	if b.Policy.ExpiresIn != "" {
		d, err := ParseExpiry(b.Policy.ExpiresIn)
		if err != nil || d < time.Minute || d > 365*24*time.Hour {
			return errors.New("expires_in must be between 1 minute and 365 days")
		}
	}
	if b.Policy.DestroyGracePeriod != "" {
		d, err := ParseExpiry(b.Policy.DestroyGracePeriod)
		if err != nil || d < time.Minute || d > 14*24*time.Hour {
			return errors.New("destroy_grace_period must be between 1 minute and 14 days")
		}
	}
	return nil
}

func ParseExpiry(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		return time.Duration(n) * 24 * time.Hour, err
	}
	return time.ParseDuration(s)
}

func (b Blueprint) Order() ([]Component, error) {
	byID := map[string]Component{}
	for _, c := range b.Components {
		byID[c.ID] = c
	}
	state := map[string]int{}
	ordered := []Component{}
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("dependency cycle at %s", id)
		}
		if state[id] == 2 {
			return nil
		}
		c, ok := byID[id]
		if !ok {
			return fmt.Errorf("unknown dependency %s", id)
		}
		state[id] = 1
		deps := append([]string(nil), c.DependsOn...)
		sort.Strings(deps)
		for _, dep := range deps {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[id] = 2
		ordered = append(ordered, c)
		return nil
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

// Digest identifies the blueprint's content; equal blueprints share a digest.
func (b Blueprint) Digest() string {
	if b.Bindings == nil {
		b.Bindings = []Binding{}
	}
	canonical, _ := json.Marshal(b)
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

func NewPlan(b Blueprint, name string, existing *Environment, now time.Time) (Plan, error) {
	if err := b.Validate(); err != nil {
		return Plan{}, err
	}
	if !identifier.MatchString(name) {
		return Plan{}, errors.New("invalid environment name")
	}
	order, _ := b.Order()
	p := Plan{Environment: name, BlueprintDigest: b.Digest(), CreatedAt: now.UTC(), ExpiresAt: now.Add(15 * time.Minute).UTC(), Bindings: b.Bindings, Protect: b.Policy.Protect}
	if b.Policy.ExpiresIn != "" {
		d, _ := ParseExpiry(b.Policy.ExpiresIn)
		t := now.Add(d).UTC()
		p.ResourceExpiresAt = &t
	}
	for _, c := range order {
		kind := "create"
		reason := "managed resource absent"
		if c.Ownership == Referenced {
			kind, reason = "attach", "referenced resource; never mutated"
		}
		if existing != nil {
			if r, ok := existing.Resources[c.ID]; ok {
				if r.Adapter != c.Adapter || r.Ownership != c.Ownership {
					return Plan{}, fmt.Errorf("%s: ownership or adapter change requires a new environment", c.ID)
				}
				kind, reason = "preserve", "resource already tracked"
			}
		}
		p.Actions = append(p.Actions, Action{Component: c, Kind: kind, Reason: reason})
	}
	if existing != nil {
		for id := range existing.Resources {
			if _, ok := findComponent(order, id); !ok {
				return Plan{}, fmt.Errorf("resource %s removed from blueprint; explicit destroy required", id)
			}
		}
	}
	unsigned, _ := json.Marshal(p)
	id := sha256.Sum256(unsigned)
	p.ID = hex.EncodeToString(id[:16])
	return p, nil
}

func findComponent(cs []Component, id string) (Component, bool) {
	for _, c := range cs {
		if c.ID == id {
			return c, true
		}
	}
	return Component{}, false
}
