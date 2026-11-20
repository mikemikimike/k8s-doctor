// Package cli implements the k8s-doctor command line.
package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/rbalukja15/k8s-doctor/internal/report"
	"github.com/rbalukja15/k8s-doctor/internal/rules"
	"github.com/rbalukja15/k8s-doctor/internal/rules/checks"
	"github.com/rbalukja15/k8s-doctor/internal/version"
)

const usage = `k8s-doctor: a read-only SRE health check for Kubernetes clusters and manifests.

Usage:
  k8s-doctor check [flags]     (or just: k8s-doctor [flags])
  k8s-doctor <command>

Commands:
  check     Analyze a snapshot, cluster or manifests folder and report findings
  snapshot  Collect a cluster or manifests folder and print the snapshot JSON
  rules     List the built-in rules
  schema    Print the JSON Schema of the --format json report
  version   Print the version
  help      Show this help

Run "k8s-doctor check -h" for check flags and exit codes.
`

// Exit codes. Codes 10-13 mean the run succeeded and found problems at or
// above --fail-on; the code says how bad the worst one is.
const (
	ExitOK            = 0
	ExitError         = 1
	ExitUsage         = 2
	ExitFindingLow    = 10
	ExitFindingMedium = 11
	ExitFindingHigh   = 12
	ExitFindingCrit   = 13
)

// ExitCodeFor returns the exit code for a worst finding severity.
func ExitCodeFor(s rules.Severity) int {
	switch s {
	case rules.SeverityCritical:
		return ExitFindingCrit
	case rules.SeverityHigh:
		return ExitFindingHigh
	case rules.SeverityMedium:
		return ExitFindingMedium
	default:
		return ExitFindingLow
	}
}

// Env is the process environment the CLI runs in, injectable for tests.
type Env struct {
	Stdout, Stderr io.Writer
	Getenv         func(string) string
}

// Run executes the CLI with args (without the program name) and returns the
// process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	return RunEnv(args, Env{Stdout: stdout, Stderr: stderr, Getenv: os.Getenv})
}

// RunEnv is Run with an explicit environment.
func RunEnv(args []string, env Env) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stdout, usage)
		return ExitOK
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, usage)
		return ExitOK
	case "version", "--version":
		fmt.Fprintln(env.Stdout, "k8s-doctor", version.Version)
		return ExitOK
	case "rules":
		return listRules(env.Stdout)
	case "schema":
		if _, err := env.Stdout.Write(report.Schema); err != nil {
			return ExitError
		}
		return ExitOK
	case "check":
		return check(args[1:], env)
	case "snapshot":
		return snapshotCmd(args[1:], env)
	}
	// Bare flags mean check: `k8s-doctor --manifests ./deploy`.
	if strings.HasPrefix(args[0], "-") {
		return check(args, env)
	}
	fmt.Fprintf(env.Stderr, "unknown command %q\n\n%s", args[0], usage)
	return ExitUsage
}

func listRules(stdout io.Writer) int {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSEVERITY\tCATEGORY\tMODES")
	for _, r := range checks.Registry().All() {
		modes := make([]string, len(r.Modes()))
		for i, m := range r.Modes() {
			modes[i] = string(m)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.ID(), r.Severity(), r.Category(), strings.Join(modes, ","))
	}
	if err := tw.Flush(); err != nil {
		return ExitError
	}
	return ExitOK
}
