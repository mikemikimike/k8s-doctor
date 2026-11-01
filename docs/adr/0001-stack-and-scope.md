# ADR-0001: Stack and scope

- **Status:** Accepted
- **Date:** 2026-10-21
- **Issue:** #3 (decision first made in #1)

## Context

k8s-doctor diagnoses Kubernetes workloads from two inputs: a live cluster and a folder of manifests or kustomize overlays. It finds misconfigurations deterministically, explains them in the context of the workload, and drafts fixes. Four questions shape everything after this:

1. Which language?
2. What is the tool allowed to change?
3. What is the split of work between deterministic rules and an LLM?
4. What data may leave the machine when an LLM is used?

## Decision

### 1. Language: Go

The libraries this tool leans on are Go libraries that can be called in-process instead of shelled out to:

- **client-go** for read-only cluster access, including metadata-only reads of Secrets (`PartialObjectMetadata`).
- **kustomize's `krusty` API** to build overlays exactly as `kustomize build` does.
- **kubeconform** to validate drafted patches against offline schemas.

Go also ships as one static binary per platform, which is what an SRE wants to drop into a CI job or a jump host. Python was the alternative: faster to prototype LLM features, but every Kubernetes integration above would become a subprocess or a less faithful reimplementation.

### 2. Read-only, always

k8s-doctor never mutates a cluster.

- The cluster collector only uses `get`, `list` and `watch`. It ships a ClusterRole with only those verbs and is tested under it (#7).
- Patches are written as files or diffs. There is no `apply` code path.
- The only write the project ever performs is opening a pull request against a manifests repository (#24). It is opt-in (`--open-pr`), off by default, and needs a token scoped to that one repository.

### 3. Rules find, the LLM explains

- **Deterministic rules** produce every finding. Each finding has a stable ID derived from the rule, the resource and the field path (ADR-0002, #10).
- **The LLM** only explains findings (#16) and narrates groups of findings that were grouped deterministically (#17). It may draft a patch only where no rule template can (#18), and every patch must pass schema validation and a re-run of the rules.
- An LLM output that cites a finding ID that does not exist, or a resource outside that finding, is rejected and replaced by the rule's static description. "The LLM never invents a finding" is therefore a checked property, not a hope, and `make eval-llm` reports its pass rate (#21).

### 4. LLM data policy and `--no-llm`

Manifests and cluster state carry environment values, tokens and internal hostnames. The policy (implemented in #26):

- **Never collected:** Secret values. Secrets and ConfigMaps are recorded by name and key only. Environment variables are recorded by name and source type, never by literal value.
- **Masked before any prompt:** annotations, Event messages, hostnames and anything else that may embed identifiers, by a redaction layer that every prompt passes through.
- **Minimal context:** a prompt contains one finding (or one group) and only the resource fields it references, not the whole snapshot.
- **`--no-llm`:** the whole pipeline (collect, rules, report) runs with no network calls. Explanations fall back to each rule's static description. The eval harness runs in this mode by default.
- **Provider-agnostic:** the LLM client is an interface. Provider and model come from config; endpoint and API key come from fixed environment variables and calls require explicit `--llm` opt-in. Prompts are not cached, so cluster context is not retained between requests or runs.

## Consequences

- The tool is useful, and fully testable, with no LLM and no network. The LLM adds explanation quality, not detection.
- Detection quality is measured on deterministic output (precision/recall over labeled fixtures), so numbers are reproducible.
- Go means the LLM integration is hand-written against provider HTTP APIs rather than using a large Python SDK ecosystem. That is acceptable because the LLM surface is small: explain, narrate, draft.
- Any feature that would write to a cluster is out of scope by definition and needs a new ADR to reconsider.
