package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rbalukja15/k8s-doctor/internal/collector"
	"github.com/rbalukja15/k8s-doctor/internal/config"
	"github.com/rbalukja15/k8s-doctor/internal/report"
	"github.com/rbalukja15/k8s-doctor/internal/rules"
	"github.com/rbalukja15/k8s-doctor/internal/rules/checks"
	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
	"github.com/rbalukja15/k8s-doctor/internal/version"
)

const checkUsage = `Usage: k8s-doctor check [flags]

Analyze one input and report findings, most severe first. Exactly one of
--snapshot, --cluster or --manifests is required.

Exit codes:
  0   no findings at or above --fail-on
  1   error (bad input, unreadable config, failed write)
  2   usage error
  10  worst finding at or above --fail-on is low
  11  ... medium
  12  ... high
  13  ... critical

Flags:
`

// errUsage marks errors that should exit with ExitUsage.
var errUsage = errors.New("usage")

// listFlag is a repeatable, comma-separated string flag.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}

type checkOptions struct {
	snapshotPath string
	cluster      bool
	manifests    string
	configPath   string
	format       string
	reportPath   string
	failOn       string
	color        string
	noLLM        bool
	explain      bool
	enable       listFlag
	disable      listFlag
}

func check(args []string, env Env) int {
	var o checkOptions
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, checkUsage)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.snapshotPath, "snapshot", "", "analyze a snapshot JSON file")
	fs.BoolVar(&o.cluster, "cluster", false, "analyze the cluster of the current kubeconfig context (read-only)")
	fs.StringVar(&o.manifests, "manifests", "", "analyze a folder of plain manifests, or a kustomize overlay built in-process")
	fs.StringVar(&o.configPath, "config", "", "config file (default ./"+config.DefaultFileName+" if present)")
	fs.StringVar(&o.format, "format", "text", "output format: text or json")
	fs.StringVar(&o.reportPath, "report", "", "also write the JSON report to this file")
	fs.StringVar(&o.failOn, "fail-on", "high", "lowest severity that makes the exit code non-zero: critical, high, medium, low or none")
	fs.StringVar(&o.color, "color", "auto", "colorize text output: auto, always or never (NO_COLOR is honored)")
	fs.BoolVar(&o.noLLM, "no-llm", false, "never call an LLM; explanations come from rule descriptions")
	fs.BoolVar(&o.explain, "explain", false, "print each finding's explanation in text output")
	fs.Var(&o.enable, "enable-rule", "enable rules by ID (repeatable, comma-separated)")
	fs.Var(&o.disable, "disable-rule", "disable rules by ID (repeatable, comma-separated)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(env.Stderr, "check: unexpected argument %q\n", fs.Arg(0))
		return ExitUsage
	}

	code, err := runCheck(&o, env)
	if err != nil {
		fmt.Fprintln(env.Stderr, "check:", err)
		if errors.Is(err, errUsage) {
			return ExitUsage
		}
		return ExitError
	}
	return code
}

func runCheck(o *checkOptions, env Env) (int, error) {
	failOn, err := parseFailOn(o.failOn)
	if err != nil {
		return 0, err
	}
	if o.format != "text" && o.format != "json" {
		return 0, fmt.Errorf("%w: --format must be text or json, got %q", errUsage, o.format)
	}
	color, err := useColor(o.color, env)
	if err != nil {
		return 0, err
	}

	snap, err := loadInput(o)
	if err != nil {
		return 0, err
	}
	cfg, err := loadConfig(o.configPath)
	if err != nil {
		return 0, err
	}
	if err := applyRuleFlags(cfg, o.enable, o.disable); err != nil {
		return 0, err
	}

	reg := checks.Registry()
	engine, err := rules.NewEngine(reg, cfg)
	if err != nil {
		return 0, err
	}
	res := engine.Run(snap)
	// The LLM layer (#26) does not exist yet, so every run is effectively
	// --no-llm and the report says so.
	rep := report.Build(snap, reg, engine, res, report.Options{
		Version: version.Version,
		Now:     time.Now(),
		LLM:     false,
	})

	if o.reportPath != "" {
		if err := writeReportFile(rep, o.reportPath); err != nil {
			return 0, err
		}
	}
	if o.format == "json" {
		err = rep.WriteJSON(env.Stdout)
	} else {
		err = rep.WriteText(env.Stdout, report.TextOptions{Color: color, Explain: o.explain})
	}
	if err != nil {
		return 0, err
	}

	worst, found := rep.WorstSeverity()
	if !found || failOn == 0 || worst < failOn {
		return ExitOK, nil
	}
	return ExitCodeFor(worst), nil
}

// parseFailOn returns the threshold severity, or 0 for "none".
func parseFailOn(v string) (rules.Severity, error) {
	if strings.EqualFold(v, "none") {
		return 0, nil
	}
	s, err := rules.ParseSeverity(v)
	if err != nil {
		return 0, fmt.Errorf("%w: --fail-on: %w", errUsage, err)
	}
	return s, nil
}

func loadInput(o *checkOptions) (*snapshot.Snapshot, error) {
	n := 0
	for _, set := range []bool{o.snapshotPath != "", o.cluster, o.manifests != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		return nil, fmt.Errorf("%w: exactly one of --snapshot, --cluster or --manifests is required", errUsage)
	}
	switch {
	case o.cluster:
		return nil, errors.New("--cluster needs the cluster collector, which is not built yet (#7)")
	case o.manifests != "":
		return collector.Manifests(o.manifests)
	default:
		return snapshot.Load(o.snapshotPath)
	}
}

func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		return config.LoadDefault(".")
	}
	return config.Load(path)
}

// applyRuleFlags layers --enable-rule and --disable-rule over the config
// file. A rule named in both is an error rather than a silent precedence.
func applyRuleFlags(cfg *config.Config, enable, disable []string) error {
	if len(enable)+len(disable) == 0 {
		return nil
	}
	if cfg.Rules == nil {
		cfg.Rules = map[string]config.RuleConfig{}
	}
	disabled := map[string]bool{}
	for _, id := range disable {
		disabled[id] = true
	}
	set := func(id string, on bool) {
		rc := cfg.Rules[id]
		rc.Enabled = &on
		cfg.Rules[id] = rc
	}
	for _, id := range enable {
		if disabled[id] {
			return fmt.Errorf("%w: rule %q is both enabled and disabled", errUsage, id)
		}
		set(id, true)
	}
	for _, id := range disable {
		set(id, false)
	}
	return nil
}

func useColor(mode string, env Env) (bool, error) {
	switch mode {
	case "always":
		return true, nil
	case "never":
		return false, nil
	case "auto":
		if env.Getenv != nil && env.Getenv("NO_COLOR") != "" {
			return false, nil
		}
		f, ok := env.Stdout.(*os.File)
		if !ok {
			return false, nil
		}
		info, err := f.Stat()
		return err == nil && info.Mode()&os.ModeCharDevice != 0, nil
	default:
		return false, fmt.Errorf("%w: --color must be auto, always or never, got %q", errUsage, mode)
	}
}

func writeReportFile(rep *report.Report, path string) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	return rep.WriteJSON(f)
}
