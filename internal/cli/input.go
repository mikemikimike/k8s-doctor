package cli

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/rbalukja15/k8s-doctor/internal/collector"
	"github.com/rbalukja15/k8s-doctor/internal/snapshot"
)

// inputOptions selects what to analyze. check and snapshot share them.
type inputOptions struct {
	snapshotPath string
	cluster      bool
	kubeconfig   string
	kubeContext  string
	namespaces   listFlag
	skipSecrets  bool
	timeout      time.Duration
	manifests    string
	// allowSnapshot is false for the snapshot command, whose output a
	// snapshot file would merely copy.
	allowSnapshot bool
}

func (i *inputOptions) register(fs *flag.FlagSet, allowSnapshot bool) {
	i.allowSnapshot = allowSnapshot
	if allowSnapshot {
		fs.StringVar(&i.snapshotPath, "snapshot", "", "analyze a snapshot JSON file")
	}
	fs.BoolVar(&i.cluster, "cluster", false, "read a live cluster, read-only (see deploy/rbac for the minimal ClusterRole)")
	fs.StringVar(&i.kubeconfig, "kubeconfig", "", "kubeconfig for --cluster (default $KUBECONFIG or ~/.kube/config)")
	fs.StringVar(&i.kubeContext, "context", "", "kubeconfig context for --cluster (default: current context)")
	fs.Var(&i.namespaces, "namespace", "namespaces for --cluster (repeatable, comma-separated; default all but system namespaces)")
	fs.BoolVar(&i.skipSecrets, "skip-secrets", false, "with --cluster, do not list Secrets, so the identity needs no access to them")
	fs.DurationVar(&i.timeout, "timeout", 2*time.Minute, "time limit for collecting from the cluster")
	fs.StringVar(&i.manifests, "manifests", "", "read a folder of plain manifests, or a kustomize overlay built in-process")
}

func (i *inputOptions) load() (*snapshot.Snapshot, error) {
	n := 0
	for _, set := range []bool{i.snapshotPath != "", i.cluster, i.manifests != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		if i.allowSnapshot {
			return nil, fmt.Errorf("%w: exactly one of --snapshot, --cluster or --manifests is required", errUsage)
		}
		return nil, fmt.Errorf("%w: exactly one of --cluster or --manifests is required", errUsage)
	}
	if !i.cluster && (i.kubeconfig != "" || i.kubeContext != "" || len(i.namespaces) > 0 || i.skipSecrets) {
		return nil, fmt.Errorf("%w: --kubeconfig, --context, --namespace and --skip-secrets only apply to --cluster", errUsage)
	}
	switch {
	case i.cluster:
		opts := collector.ClusterOptions{Kubeconfig: i.kubeconfig, Context: i.kubeContext, Namespaces: i.namespaces, SkipSecrets: i.skipSecrets}
		clients, err := collector.NewClients(opts)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), i.timeout)
		defer cancel()
		return collector.Cluster(ctx, clients, opts)
	case i.manifests != "":
		return collector.Manifests(i.manifests)
	default:
		return snapshot.Load(i.snapshotPath)
	}
}
