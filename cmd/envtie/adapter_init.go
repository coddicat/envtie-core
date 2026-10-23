package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
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
	SDKVersion string
}

func adapterCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: adapter init <name> | adapter build [dir] | adapter test <executable>")
	}
	switch args[0] {
	case "test":
		if len(args) != 2 {
			return errors.New("usage: adapter test <executable>")
		}
		return testAdapter(args[1])
	case "init":
		return initAdapter(args[1:])
	case "build":
		return buildAdapterCommand(args[1:])
	case "help", "-h", "--help":
		fmt.Print(`Custom adapters

  adapter init <name> [flags]   Scaffold, fetch the SDK, build, and print the SHA-256 pin
  adapter build [dir] [flags]   Rebuild an existing scaffold (default dir: .)
  adapter test <executable>     Validate a binary and print its SHA-256 pin

Init flags:
  -dir string       Output directory (default ./adapter-<name>)
  -module string    Go module path (default example.com/adapter-<name>)
  -force            Overwrite files that already exist
  -no-build         Only write files (skip go get / tidy / build / test)
  -no-test          Build but skip adapter test

Build flags:
  -no-test          Rebuild without running adapter test

Requires the Go toolchain (https://go.dev/dl/). Then pin executable + sha256
in the dashboard Custom adapter step and put provider tokens on the runner.
`)
		return nil
	default:
		return errors.New("usage: adapter init <name> | adapter build [dir] | adapter test <executable>")
	}
}

func initAdapter(args []string) error {
	f := flag.NewFlagSet("adapter init", flag.ContinueOnError)
	f.SetOutput(os.Stderr)
	dirFlag := f.String("dir", "", "output directory")
	moduleFlag := f.String("module", "", "Go module path")
	force := f.Bool("force", false, "overwrite existing files")
	noBuild := f.Bool("no-build", false, "skip go get, tidy, build, and test")
	noTest := f.Bool("no-test", false, "build but skip adapter test")
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
		return errors.New("usage: adapter init <name> [-dir path] [-module path] [-force] [-no-build] [-no-test]")
	case name == "" && len(rest) == 1:
		name = strings.TrimSpace(rest[0])
	case name == "":
		return errors.New("usage: adapter init <name> [-dir path] [-module path] [-force] [-no-build] [-no-test]")
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
	replace := findLocalEnvtieCore(dir)
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
		Replace:    replace,
		SDKVersion: sdkRequireVersion(replace != ""),
	}
	if err := writeAdapterScaffold(sc, *force); err != nil {
		return err
	}
	fmt.Printf("created %s\n", sc.Dir)
	if *noBuild {
		printAdapterManualNextSteps(sc)
		return nil
	}
	binPath, err := buildAdapter(sc.Dir, sc.Binary, replace != "")
	if err != nil {
		return fmt.Errorf("%w\nfiles were written to %s; fix the issue then run: envtie adapter build", err, sc.Dir)
	}
	fmt.Printf("built %s\n", binPath)
	if !*noTest {
		if err := testAdapter(binPath); err != nil {
			return fmt.Errorf("%w\nbinary is at %s; re-run: envtie adapter test %s", err, binPath, binPath)
		}
	}
	fmt.Printf("next: pin adapter %s (executable %s) in the dashboard Custom adapter step\n", sc.AdapterID, adapterExecHint(sc.Binary))
	fmt.Printf("runner env: %s and %s\n", sc.StateEnv, sc.SecretEnv)
	return nil
}

func buildAdapterCommand(args []string) error {
	f := flag.NewFlagSet("adapter build", flag.ContinueOnError)
	f.SetOutput(os.Stderr)
	noTest := f.Bool("no-test", false, "rebuild without running adapter test")
	dir := "."
	flagArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		dir = args[0]
		flagArgs = args[1:]
	}
	if err := f.Parse(flagArgs); err != nil {
		return err
	}
	if len(f.Args()) > 0 {
		return errors.New("usage: adapter build [dir] [-no-test]")
	}
	if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
		return fmt.Errorf("adapter build: %s is not an adapter scaffold (missing main.go)", dir)
	}
	binary, err := detectAdapterBinaryName(dir)
	if err != nil {
		return err
	}
	binPath, err := buildAdapter(dir, binary, goModHasLocalReplace(dir))
	if err != nil {
		return err
	}
	fmt.Printf("built %s\n", binPath)
	if !*noTest {
		return testAdapter(binPath)
	}
	return nil
}

func goModHasLocalReplace(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\r"))
		if strings.HasPrefix(line, "replace ") && strings.Contains(line, "github.com/coddicat/envtie-core") {
			return true
		}
	}
	return false
}

func detectAdapterBinaryName(dir string) (string, error) {
	if entries, err := os.ReadDir(filepath.Join(dir, "dist")); err == nil {
		var candidates []string
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasPrefix(name, "adapter-") {
				continue
			}
			candidates = append(candidates, strings.TrimSuffix(name, ".exe"))
		}
		sort.Strings(candidates)
		if len(candidates) == 1 {
			return candidates[0], nil
		}
		if len(candidates) > 1 {
			// Prefer the directory/module name when several artifacts exist.
			base := filepath.Base(dir)
			for _, name := range candidates {
				if name == base {
					return name, nil
				}
			}
			return candidates[0], nil
		}
	}
	base := filepath.Base(filepath.Clean(dir))
	if strings.HasPrefix(base, "adapter-") {
		return base, nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("adapter build: could not detect binary name in %s (expected dist/adapter-* or adapter-* directory)", dir)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == "module" {
			mod := fields[1]
			if i := strings.LastIndex(mod, "/"); i >= 0 {
				mod = mod[i+1:]
			}
			if strings.HasPrefix(mod, "adapter-") {
				return mod, nil
			}
		}
	}
	return "", fmt.Errorf("adapter build: could not detect binary name in %s", dir)
}

func printAdapterManualNextSteps(sc adapterScaffold) {
	bin := adapterBinaryFile(sc.Binary)
	fmt.Printf("next:\n")
	fmt.Printf("  cd %s\n", sc.Dir)
	fmt.Printf("  envtie adapter build\n")
	fmt.Printf("  # or: go get github.com/coddicat/envtie-core@%s && go mod tidy && go build -trimpath -o dist/%s .\n", sdkModuleQuery(), bin)
	fmt.Printf("then pin adapter %s in the dashboard Custom adapter step\n", sc.AdapterID)
}

func adapterExecHint(binary string) string {
	return "./dist/" + strings.TrimSuffix(binary, ".exe")
}

func adapterBinaryFile(binary string) string {
	name := strings.TrimSuffix(binary, ".exe")
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// sdkModuleQuery is the go get @query matching this CLI release (v-prefixed or latest).
func sdkModuleQuery() string {
	v := strings.TrimSpace(version)
	if v == "" || v == "dev" {
		return "latest"
	}
	if !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

func sdkRequireVersion(localReplace bool) string {
	if localReplace {
		return "v0.0.0"
	}
	q := sdkModuleQuery()
	if q == "latest" {
		return "v0.0.0"
	}
	return q
}

func buildAdapter(dir, binary string, localReplace bool) (string, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return "", errors.New("go toolchain required: install Go from https://go.dev/dl/ then re-run (or pass -no-build)")
	}
	if err := os.MkdirAll(filepath.Join(dir, "dist"), 0755); err != nil {
		return "", err
	}
	outName := adapterBinaryFile(binary)
	outPath := filepath.Join("dist", outName)
	run := func(args ...string) error {
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = append(os.Environ(), "GOTOOLCHAIN=auto")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
		}
		return nil
	}
	if !localReplace {
		mod := "github.com/coddicat/envtie-core@" + sdkModuleQuery()
		fmt.Printf("fetching %s\n", mod)
		if err := run("get", mod); err != nil {
			return "", err
		}
	}
	if err := run("mod", "tidy"); err != nil {
		return "", err
	}
	fmt.Printf("compiling %s\n", outPath)
	if err := run("build", "-trimpath", "-o", outPath, "."); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(filepath.Join(dir, outPath))
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", err
	}
	return abs, nil
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

require github.com/coddicat/envtie-core {{.SDKVersion}}
{{if .Replace}}
replace github.com/coddicat/envtie-core => {{.Replace}}
{{end}}`

const adapterGitignoreTemplate = `dist/
*.exe
`

const adapterMainTemplate = `// Private EnvTie.dev adapter scaffolded by "envtie adapter init".
// Rebuild with "envtie adapter build", then pin executable + sha256 in the dashboard Custom adapter step.
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

` + "`" + `envtie adapter init` + "`" + ` already runs ` + "`" + `go get` + "`" + `, ` + "`" + `go mod tidy` + "`" + `, ` + "`" + `go build` + "`" + `, and ` + "`" + `adapter test` + "`" + ` when the Go toolchain is installed. After you edit the code:

` + "```" + `powershell
envtie adapter build
` + "```" + `

Copy the printed ` + "`" + `sha256:` + "`" + ` into the dashboard Custom adapter step.

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
| SHA-256 pin | from ` + "`" + `adapter init` + "`" + ` / ` + "`" + `adapter build` + "`" + ` / ` + "`" + `adapter test` + "`" + ` |
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
