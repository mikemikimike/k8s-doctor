Manifests for the cross-mode test (#9). `crossmode_test.go` collects them in
manifest mode and in cluster mode, and the two `spec` sections must match.
`test/kind/run.sh` applies them to kind with `kubectl apply -f`, so they must
stay valid against a real API server. Server-populated fields are left unset
on purpose: that is what the comparison has to see through.
