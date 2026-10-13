// Package githubsetup contains the customer-owned GitHub App contract used by
// the CLI. It does not create apps or retain customer credentials.
package githubsetup

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// AppPermissions is the least-privilege repository permission set required by
// the GitHub environment adapter and pull-request workflow.
var AppPermissions = map[string]string{
	"metadata":       "read",
	"actions":        "read",
	"administration": "write",
	"environments":   "write",
	"secrets":        "write",
	"deployments":    "write",
	"pull_requests":  "read",
}

// AppManifest returns a GitHub App manifest whose private key is issued
// directly to the customer.
func AppManifest(name, redirectURL string) (string, error) {
	if name == "" || len(name) > 34 || strings.ContainsAny(name, "\"\\\r\n") || strings.ContainsAny(redirectURL, "\"\\\r\n ") {
		return "", errors.New("invalid manifest")
	}
	if !strings.HasPrefix(redirectURL, "https://") && !strings.HasPrefix(redirectURL, "http://localhost") && !strings.HasPrefix(redirectURL, "http://127.0.0.1") {
		return "", errors.New("invalid manifest")
	}
	keys := make([]string, 0, len(AppPermissions))
	for key := range AppPermissions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, fmt.Sprintf("    %q: %q", key, AppPermissions[key]))
	}
	return fmt.Sprintf(`{
  "name": %q,
  "url": "https://envtie.dev",
  "redirect_url": %q,
  "public": false,
  "default_permissions": {
%s
  }
}`, name, redirectURL, strings.Join(lines, ",\n")), nil
}
