package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
)

const snapshotUsage = `Usage: k8s-doctor snapshot (--cluster | --manifests <dir>) [flags]

Collect a snapshot and print it as JSON, without running any rules. Use it
to inspect exactly what k8s-doctor sees, or as input to "check --snapshot"
and fixtures. It never contains Secret or ConfigMap values or env literals,
but in cluster mode it does contain Event messages as written by controllers,
object names and labels, and the kubeconfig context name. Review it before
sharing it outside your organization.

Flags:
`

func snapshotCmd(args []string, env Env) int {
	var in inputOptions
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, snapshotUsage)
		fs.PrintDefaults()
	}
	in.register(fs, false)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(env.Stderr, "snapshot: unexpected argument %q\n", fs.Arg(0))
		return ExitUsage
	}
	snap, err := in.load()
	if err != nil {
		fmt.Fprintln(env.Stderr, "snapshot:", err)
		if errors.Is(err, errUsage) {
			return ExitUsage
		}
		return ExitError
	}
	enc := json.NewEncoder(env.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snap); err != nil {
		fmt.Fprintln(env.Stderr, "snapshot:", err)
		return ExitError
	}
	return ExitOK
}
