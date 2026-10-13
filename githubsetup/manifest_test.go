package githubsetup

import (
	"encoding/json"
	"testing"
)

func TestAppManifestPermissions(t *testing.T) {
	raw, err := AppManifest("envtie-test", "https://envtie.dev/github/setup")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Permissions map[string]string `json:"default_permissions"`
	}
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Permissions["actions"] != "read" || manifest.Permissions["administration"] != "write" {
		t.Fatalf("permissions %#v", manifest.Permissions)
	}
}
