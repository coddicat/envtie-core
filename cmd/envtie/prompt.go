package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/coddicat/envtie-core/sdk/adapter"
	"golang.org/x/term"
)

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
			var raw []byte
			raw, err = term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			value = string(raw)
			for i := range raw {
				raw[i] = 0
			}
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
