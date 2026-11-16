// Package collector turns a folder of manifests or kustomize overlays
// (Manifests) and, from #7, a live cluster into a snapshot.Snapshot.
// Collectors are read-only and never keep Secret values or env literals.
package collector
