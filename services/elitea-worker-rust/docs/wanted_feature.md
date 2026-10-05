# Features wanted after Rust worker completion

This backlog contains work outside the active Rust worker completion gates.
Complete and release the full worker before starting these features.
The user sets this boundary on 2026-10-05.

## WF-01 — Code workspaces

Status: deferred. This item replaces the former Point 5f extension.
It does not block the current Code node, Point 5, or the full worker release.

The current Code node retains notebook-cell behavior: source, selected state, dependencies, and returned output.
Workspace access is an optional future capability.

Future scope includes:

- Immutable repository snapshots through an authorized saved toolkit.
- Trusted Main descriptor metadata and matching YAML/editor selection controls.
- Operator-approved folders, Docker volumes, and Kubernetes PVC paths.
- Project-scoped source access, immutable revisions, and path containment.
- Read-only access by default. Explicit write access requires policy, bounded storage, and durable effect handling.
- Resource limits, cleanup, and recovery across Worker, Main, and Supervisor replacement.

Scratch directories and Kubernetes `emptyDir` storage do not satisfy mounted workspace access.
The future feature must use approved source identities rather than arbitrary host paths or volume names.

Preserve the existing workspace source and mappings for later integration.
Deliver working shared foundation code with the current sandbox.
Do not remove or stash code that the current Code node needs.
The deferral changes feature scope and acceptance, not working runtime contracts.
Main already computes reference identities from resolved toolkit references and rechecks them during acquisition.
The UI/API still lacks trusted selection metadata. No new descriptor endpoint is delivered in the current Code work.

Later acceptance must cover repository snapshots, Docker volumes, Kubernetes PVCs, denied sources, explicit write policy, and YAML/editor parity.
It must retain admitted source identity and revision during recovery and prove cleanup.

Existing source history:

- [Repository workspace mapping](source-mapping/code-repository-workspace-20261004.md)
- [Workspace activation mapping](source-mapping/code-workspace-activation-20261005.md)
- [Combined Code ownership mapping](source-mapping/code-root-composition-20261005.md)

The active [remaining gaps](remaining-gates.md) and [acceptance register](testing-gaps.md) exclude this future scope.
