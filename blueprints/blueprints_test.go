package blueprints

import (
	"testing"

	"github.com/coddicat/envtie-core/core"
)

func TestCanonicalTemplates(t *testing.T) {
	for _, name := range Names() {
		raw, err := Get(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := core.ParseBlueprint([]byte(raw)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
