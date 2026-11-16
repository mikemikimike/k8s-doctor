# Fixtures

Labeled inputs with known expected findings. They are the ground truth for `make test` today and for `make eval` (#21).

## Layout

```text
fixtures/
├── _bases/                       # shared kustomize bases, not fixtures
├── clean/<name>/                 # no findings from ANY rule
├── kustomize/<name>/             # overlays exercising the manifest collector
└── <rule-id>/
    ├── positive/                 # the rule must fire, exactly as listed
    └── near-miss/                # looks like a problem, isn't: the rule stays quiet
```

Every directory containing an `expected.yaml` is a fixture. Next to it is the input:

- `snapshot.json`: a snapshot in the format from [ADR-0002](../docs/adr/0002-collector-and-snapshot.md). Required for cluster-only rules, which need the `runtime` section.
- `manifests/`: plain manifests or a kustomize overlay, read through the manifest collector exactly as `--manifests` would. Overlays can share bases from `fixtures/_bases/`, which holds no `expected.yaml` and so is not a fixture itself.

## `expected.yaml`

```yaml
description: "One sentence on what this fixture contains."
kind: positive            # positive | clean | near-miss
rules:                    # optional: only compare findings from these rules
  - probe-readiness-missing
findings:
  - rule: probe-readiness-missing
    resource: Deployment/shop/orders       # Kind/namespace/name, or Kind/name
    field: spec.template.spec.containers[name=app].readinessProbe
```

Matching is exact on rule + resource + field, the same inputs as the stable finding ID. Any missing finding is a false negative and any extra finding is a false positive; both fail the test.

- `clean` fixtures must list no findings and should leave `rules` empty, so they guard against false positives from every rule.
- `positive` fixtures must list at least one finding.
- `near-miss` fixtures usually list none; they exist to catch false positives on inputs that look wrong but are not (a limit without a request, an image pinned by digest, a Job without probes).

## Adding a rule's fixtures

1. Create `fixtures/<rule-id>/positive/` and `fixtures/<rule-id>/near-miss/` with a `snapshot.json` and `expected.yaml` each, scoped to the rule with `rules:`.
2. Make sure `clean/` fixtures still pass, or extend them if the rule covers a resource kind they lack.
3. Run `make test`. `TestEveryRuleHasFixtures` fails if a registered rule has no positive, clean or near-miss fixture.
