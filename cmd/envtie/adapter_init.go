package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"text/template"
)

var adapterNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,40}$`)
var modulePathPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._\-/]{0,191}$`)

// Official catalog adapter names — private scaffolds must not collide.
var reservedAdapterNames = map[string]struct{}{
	"neon": {}, "upstash": {}, "stripe": {}, "sentry": {}, "github": {},
}

type adapterScaffold struct {
	Name       string
	Module     string
	Dir        string
	EnvPrefix  string
	SecretEnv  string
	StateEnv   string
	OutputName string
	Binary     string
	AdapterID  string
	Component  string
	Replace    string
}

func adapterCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: adapter init <name> | adapter test <executable>")
	}
	switch args[0] {
	case "test":
		if len(args) != 2 {
			return errors.New("usage: adapter test <executable>")
		}
		return testAdapter(args[1])
	case "init":
		return initAdapter(args[1:])
	case "help", "-h", "--help":
		fmt.Print(`Custom adapters

  adapter init <name> [flags]   Scaffold a private Go adapter
  adapter test <executable>     Validate a binary and print its SHA-256 pin

Init flags:
  -dir string       Output directory (default ./adapter-<name>)
  -module string    Go module path (default example.com/adapter-<name>)
  -force            Overwrite files that already exist

After init:
  cd <dir>
  go mod tidy
  go build -trimpath -o dist/adapter-<name> .
  envtie adapter test ./dist/adapter-<name>
  Pin executable + sha256 via the dashboard Custom adapter step.
`)
		return nil
	default:
		return errors.New("usage: adapter init <name> | adapter test <executable>")
	}
}

func initAdapter(args []string) error {
	f := flag.NewFlagSet("adapter init", flag.ContinueOnError)
	f.SetOutput(os.Stderr)
	dirFlag := f.String("dir", "", "output directory")
	moduleFlag := f.String("module", "", "Go module path")
	force := f.Bool("force", false, "overwrite existing files")
	name := ""
	flagArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name = strings.TrimSpace(args[0])
		flagArgs = args[1:]
	}
	if err := f.Parse(flagArgs); err != nil {
		return err
	}
	rest := f.Args()
	switch {
	case name != "" && len(rest) > 0:
		return errors.New("usage: adapter init <name> [-dir path] [-module path] [-force]")
	case name == "" && len(rest) == 1:
		name = strings.TrimSpace(rest[0])
	case name == "":
		return errors.New("usage: adapter init <name> [-dir path] [-module path] [-force]")
	}
	if !adapterNamePattern.MatchString(name) {
		return errors.New("adapter name: use a lowercase letter, then letters, numbers, or hyphens (max 41 chars)")
	}
	if _, reserved := reservedAdapterNames[name]; reserved {
		return fmt.Errorf("%q is an official EnvTie adapter; choose another name", name)
	}
	dir := strings.TrimSpace(*dirFlag)
	if dir == "" {
		dir = filepath.Join(".", "adapter-"+name)
	}
	module := strings.TrimSpace(*moduleFlag)
	if module == "" {
		module = "example.com/adapter-" + name
	}
	if !modulePathPattern.MatchString(module) || strings.Contains(module, "//") || strings.HasSuffix(module, "/") {
		return errors.New("module: use a Go module path such as example.com/adapter-myprovider")
	}
	prefix := envPrefix(name)
	sc := adapterScaffold{
		Name:       name,
		Module:     module,
		Dir:        dir,
		EnvPrefix:  prefix,
		SecretEnv:  prefix + "_API_TOKEN",
		StateEnv:   prefix + "_STATE_DIR",
		OutputName: prefix + "_VALUE",
		Binary:     "adapter-" + name,
		AdapterID:  name + "@1",
		Component:  name,
		Replace:    findLocalEnvtieCore(dir),
	}
	if err := writeAdapterScaffold(sc, *force); err != nil {
		return err
	}
	bin := sc.Binary
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	fmt.Printf("created %s\n", sc.Dir)
	fmt.Printf("next:\n")
	fmt.Printf("  cd %s\n", sc.Dir)
	if sc.Replace == "" {
		fmt.Printf("  go get github.com/coddicat/envtie-core@latest\n")
	}
	fmt.Printf("  go mod tidy\n")
	fmt.Printf("  go build -trimpath -o dist/%s .\n", bin)
	fmt.Printf("  envtie adapter test ./dist/%s\n", bin)
	fmt.Printf("then pin adapter %s in the dashboard Custom adapter step\n", sc.AdapterID)
	return nil
}

func envPrefix(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

func findLocalEnvtieCore(scaffoldDir string) string {
	start, err := filepath.Abs(".")
	if err != nil {
		return ""
	}
	for dir := start; ; dir = filepath.Dir(dir) {
		if isEnvtieCoreModule(filepath.Join(dir, "go.mod")) {
			absScaffold, err := filepath.Abs(scaffoldDir)
			if err != nil {
				return ""
			}
			rel, err := filepath.Rel(absScaffold, dir)
			if err != nil {
				return ""
			}
			return filepath.ToSlash(rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return ""
}

func isEnvtieCoreModule(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		fields := strings.Fields(line)
		return len(fields) >= 2 && fields[0] == "module" && fields[1] == "github.com/coddicat/envtie-core"
	}
	return false
}

func writeAdapterScaffold(sc adapterScaffold, force bool) error {
	files := map[string]string{
		"main.go":                adapterMainTemplate,
		"main_test.go":           adapterMainTestTemplate,
		"go.mod":                 adapterGoModTemplate,
		"README.md":              adapterReadmeTemplate,
		"blueprint.example.yaml": adapterBlueprintTemplate,
		".gitignore":             adapterGitignoreTemplate,
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	rendered := make(map[string][]byte, len(files))
	for _, name := range names {
		tmpl, err := template.New(name).Parse(files[name])
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, sc); err != nil {
			return err
		}
		rendered[name] = buf.Bytes()
	}

	if !force {
		for _, name := range names {
			path := filepath.Join(sc.Dir, name)
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("%s already exists (pass -force to overwrite)", path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(sc.Dir, "dist"), 0755); err != nil {
		return err
	}
	for _, name := range names {
		path := filepath.Join(sc.Dir, name)
		if err := os.WriteFile(path, rendered[name], 0644); err != nil {
			return err
		}
	}
	return nil
}

const adapterGoModTemplate = `module {{.Module}}

go 1.22

require github.com/coddicat/envtie-core v0.0.0
{{if .Replace}}
replace github.com/coddicat/envtie-core => {{.Replace}}
{{end}}`

const adapterGitignoreTemplate = `dist/
*.exe
`

const adapterMainTemplate = `// Private EnvTie.dev adapter scaffolded by "envtie adapter init".
// Build, pin with "envtie adapter test", then use the dashboard Custom adapter step.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"

	"github.com/coddicat/envtie-core/sdk/adapter"
)

type handler struct{}

var safe = regexp.MustCompile(` + "`" + `^[a-z][a-z0-9_-]{0,62}$` + "`" + `)

func (handler) Manifest() adapter.Manifest {
	return adapter.Manifest{
		Name:         "{{.Name}}",
		Version:      "1.0.0",
		Protocol:     1,
		Capabilities: []string{"read", "apply", "validate", "outputs", "destroy"},
		CredentialEnv: []string{"{{.SecretEnv}}"},
		Inputs: []adapter.Input{
			{Name: "{{.StateEnv}}", Kind: adapter.InputVariable, Required: true, Description: "Directory for per-environment state files"},
			{Name: "{{.SecretEnv}}", Kind: adapter.InputSecret, Required: true, Description: "Value returned as {{.OutputName}}"},
		},
		Outputs: []adapter.Field{
			{Name: "{{.OutputName}}", Sensitivity: "SECRET"},
		},
	}
}

func (handler) Handle(_ context.Context, r adapter.Request) (adapter.Response, error) {
	if !safe.MatchString(r.Environment) || !safe.MatchString(r.Component) {
		return adapter.Response{}, errors.New("invalid identity")
	}
	directory := os.Getenv("{{.StateEnv}}")
	if directory == "" {
		return adapter.Response{}, errors.New("{{.StateEnv}} is required")
	}
	id := r.Environment + "-" + r.Component
	if r.ExternalID != "" && r.ExternalID != id {
		return adapter.Response{}, errors.New("resource identity mismatch")
	}
	path := filepath.Join(directory, id+".txt")
	switch r.Hook {
	case "read", "validate":
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return adapter.Response{Absent: true}, nil
		} else if err != nil {
			return adapter.Response{}, err
		}
		return adapter.Response{ExternalID: id, Status: "ready"}, nil
	case "apply":
		if err := os.MkdirAll(directory, 0700); err != nil {
			return adapter.Response{}, err
		}
		if err := os.WriteFile(path, []byte("managed by EnvTie.dev\n"), 0600); err != nil {
			return adapter.Response{}, err
		}
		return adapter.Response{ExternalID: id, Status: "created"}, nil
	case "outputs":
		secret := os.Getenv("{{.SecretEnv}}")
		if secret == "" {
			return adapter.Response{}, errors.New("{{.SecretEnv}} is required")
		}
		return adapter.Response{Outputs: map[string]string{"{{.OutputName}}": secret}}, nil
	case "destroy":
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return adapter.Response{}, err
		}
		return adapter.Response{Absent: true}, nil
	}
	return adapter.Response{}, errors.New("unsupported hook")
}

func main() { adapter.Serve(handler{}) }
`

const adapterMainTestTemplate = `package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/coddicat/envtie-core/sdk/adapter"
)

func TestLifecycle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("{{.StateEnv}}", dir)
	t.Setenv("{{.SecretEnv}}", "secret-fixture")
	h := handler{}
	m := h.Manifest()
	if m.Name != "{{.Name}}" || m.Protocol != 1 {
		t.Fatalf("manifest: %+v", m)
	}
	req := adapter.Request{Protocol: 1, Hook: "apply", Environment: "preview01", Component: "{{.Component}}"}
	created, err := h.Handle(context.Background(), req)
	if err != nil || created.ExternalID == "" {
		t.Fatalf("apply: %+v %v", created, err)
	}
	out, err := h.Handle(context.Background(), adapter.Request{Protocol: 1, Hook: "outputs", Environment: "preview01", Component: "{{.Component}}", ExternalID: created.ExternalID})
	if err != nil || out.Outputs["{{.OutputName}}"] != "secret-fixture" {
		t.Fatalf("outputs: %+v %v", out, err)
	}
	path := filepath.Join(dir, "preview01-{{.Component}}.txt")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	gone, err := h.Handle(context.Background(), adapter.Request{Protocol: 1, Hook: "destroy", Environment: "preview01", Component: "{{.Component}}", ExternalID: created.ExternalID})
	if err != nil || !gone.Absent {
		t.Fatalf("destroy: %+v %v", gone, err)
	}
}
`

const adapterReadmeTemplate = `# {{.Binary}}

Private EnvTie.dev adapter scaffolded by ` + "`" + `envtie adapter init {{.Name}}` + "`" + `.

This binary creates one small state file per environment and exposes ` + "`" + `{{.OutputName}}` + "`" + `.
Replace the hooks with your provider's API when you are ready.

## Build and pin

` + "```" + `powershell
go mod tidy
go build -trimpath -o dist/{{.Binary}}.exe .
envtie adapter test ./dist/{{.Binary}}.exe
` + "```" + `

On macOS/Linux omit ` + "`" + `.exe` + "`" + `. Copy the printed ` + "`" + `sha256:` + "`" + ` into the dashboard.

## Runner credentials

| Variable | Purpose |
| --- | --- |
| ` + "`" + `{{.StateEnv}}` + "`" + ` | Directory for per-environment state |
| ` + "`" + `{{.SecretEnv}}` + "`" + ` | Value returned as ` + "`" + `{{.OutputName}}` + "`" + ` |

## Dashboard

Choose **Custom adapter** in the Blueprint wizard:

| Field | Value |
| --- | --- |
| Component id | ` + "`" + `{{.Component}}` + "`" + ` |
| Adapter id | ` + "`" + `{{.AdapterID}}` + "`" + ` |
| Executable path | ` + "`" + `./dist/{{.Binary}}` + "`" + ` |
| SHA-256 pin | from ` + "`" + `envtie adapter test` + "`" + ` |
| Outputs to bind | ` + "`" + `{{.OutputName}}` + "`" + ` |

See also ` + "`" + `blueprint.example.yaml` + "`" + `.
`

const adapterBlueprintTemplate = `name: {{.Name}}-preview
components:
  - id: {{.Component}}
    adapter: {{.AdapterID}}
    executable: ./dist/{{.Binary}}
    sha256: REPLACE_WITH_ADAPTER_TEST_SHA256
    ownership: managed
bindings:
  - from: {{.Component}}.{{.OutputName}}
    to: dotenv.{{.OutputName}}
policy:
  expires_in: 1d
  protect: false
`
