package main

import (
	"bufio"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/coddicat/envtie-core/sdk/adapter"
	"golang.org/x/term"
)

// promptRunnerConnection asks for the control plane URL, public key, and runner
// token when they are missing. Provider credentials are requested later, once
// an approved plan lists them. Existing valid values are left unchanged.
func promptRunnerConnection() error {
	if !runnerCanPrompt() {
		return nil
	}
	type field struct {
		name, description string
		secret            bool
		normalize         func(string) string
		valid             func(string) bool
	}
	fields := []field{
		{name: "ENVTIE_API_URL", description: "control plane URL", normalize: func(v string) string { return strings.TrimRight(strings.TrimSpace(v), "/") }, valid: secureAPIURL},
		{name: "ENVTIE_PUBLIC_KEY", description: "base64 Ed25519 public key", normalize: strings.TrimSpace, valid: validPublicKey},
		{name: "ENVTIE_RUNNER_TOKEN", description: "runner token", secret: true, normalize: strings.TrimSpace, valid: acceptableCredential},
	}
	announced := false
	for _, field := range fields {
		if field.valid(field.normalize(os.Getenv(field.name))) {
			continue
		}
		if !announced {
			fmt.Fprintln(os.Stderr, "runner: enter the control plane connection. The token stays in this process.")
			announced = true
		}
		if err := promptEnvValue(field.name, field.description, field.secret, field.normalize, field.valid); err != nil {
			return err
		}
	}
	return nil
}

func promptEnvValue(name, description string, secret bool, normalize func(string) string, valid func(string) bool) error {
	for {
		fmt.Fprintf(os.Stderr, "%s (%s): ", name, description)
		value, err := readPromptLine(secret)
		if err != nil {
			return err
		}
		value = normalize(value)
		if !valid(value) {
			fmt.Fprintln(os.Stderr, "runner: that value is not valid")
			continue
		}
		return os.Setenv(name, value)
	}
}

func validPublicKey(value string) bool {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	return err == nil && len(raw) == ed25519.PublicKeySize
}

func readPromptLine(secret bool) (string, error) {
	if secret {
		return readMasked(int(os.Stdin.Fd()))
	}
	reader := bufio.NewReader(os.Stdin)
	value, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(value, "\r\n"), nil
}

// readMasked reads a secret and prints one * per typed character.
func readMasked(fd int) (string, error) {
	state, err := term.MakeRaw(fd)
	if err != nil {
		raw, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		value := string(raw)
		for i := range raw {
			raw[i] = 0
		}
		return value, nil
	}
	defer func() { _ = term.Restore(fd, state) }()
	var buf []byte
	chunk := make([]byte, 64)
	for {
		n, err := os.Stdin.Read(chunk)
		for i := 0; i < n; i++ {
			c := chunk[i]
			next, echo, done, interrupt := maskedStep(buf, c)
			buf = next
			fmt.Fprint(os.Stderr, echo)
			if interrupt {
				fmt.Fprintln(os.Stderr)
				return "", errors.New("interrupted")
			}
			if done {
				if c == '\r' && i+1 < n && chunk[i+1] == '\n' {
					i++
				}
				value := string(buf)
				for j := range buf {
					buf[j] = 0
				}
				return value, nil
			}
		}
		if err != nil {
			return "", err
		}
	}
}

// maskedStep updates a secret buffer for one typed byte. echo is written to the terminal.
func maskedStep(buf []byte, c byte) (next []byte, echo string, done, interrupt bool) {
	switch c {
	case 3:
		return buf, "", false, true
	case '\r', '\n':
		return buf, "\r\n", true, false
	case 8, 127:
		if len(buf) == 0 {
			return buf, "", false, false
		}
		return buf[:len(buf)-1], "\b \b", false, false
	default:
		if c < 32 {
			return buf, "", false, false
		}
		return append(append([]byte{}, buf...), c), "*", false, false
	}
}

func runnerCanPrompt() bool {
	if os.Getenv("ENVTIE_RUNNER_PROMPT") == "0" || os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") == "true" {
		return false
	}
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// promptForCredentials asks once for every missing value. Secrets are masked.
// Values stay in this process and are not sent to the control plane.
func promptForCredentials(inputs []adapter.Input) bool {
	fmt.Fprintln(os.Stderr, "runner: enter the missing values. They stay in this process.")
	reader := bufio.NewReader(os.Stdin)
	for _, input := range inputs {
		if strings.TrimSpace(os.Getenv(input.Name)) != "" {
			continue
		}
		label := input.Kind
		if input.Description != "" {
			label += ", " + input.Description
		}
		fmt.Fprintf(os.Stderr, "%s (%s): ", input.Name, label)
		var value string
		var err error
		if input.Kind == adapter.InputSecret {
			value, err = readPromptLine(true)
		} else {
			value, err = reader.ReadString('\n')
			value = strings.TrimRight(value, "\r\n")
		}
		if err != nil || !acceptableCredential(value) || os.Setenv(input.Name, value) != nil {
			return false
		}
	}
	return true
}

func acceptableCredential(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 4096 && !strings.ContainsAny(value, "\r\n\x00")
}

func explainMissingCredentials(environment string, inputs []adapter.Input) {
	fmt.Fprintf(os.Stderr, "runner: %s needs values before any provider change:\n", environment)
	for _, input := range inputs {
		line := "  " + input.Name + " (" + input.Kind + ")"
		if input.Description != "" {
			line += ": " + input.Description
		}
		fmt.Fprintln(os.Stderr, line)
	}
	fmt.Fprintln(os.Stderr, "runner: the approval is still queued. Set the values and run again.")
}
