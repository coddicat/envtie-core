// Package envx reads process configuration from ENVTIE_ variables.
package envx

import "os"

// Get returns ENVTIE_<name>. An empty value is returned when the variable is set to empty.
func Get(name string) string {
	return os.Getenv("ENVTIE_" + name)
}
