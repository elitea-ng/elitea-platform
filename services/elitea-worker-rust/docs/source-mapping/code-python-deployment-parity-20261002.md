# Python preparation deployment parity

Date: 2026-10-02. Status: source and render checks pass; infrastructure acceptance remains open.

The Python delivery feature has an optional Docker preparation overlay.
Its initial Kubernetes packaging has execution RBAC and a read-only supervisor root, but no preparation namespace or content staging mounts.
This change closes those deployment omissions with optional Helm values and templates.
It retains one consolidated supervisor Deployment and the existing execution profiles.

## Current-to-new source mapping

The SDK behavior reference is revision `a54db410a46bac5e2c3cbc2db46c148a5c3d069c`, `infra/data/sandbox/main.ts::install_imports`.
The frozen repository HEAD is `ad4da99ff2dcf63b9c92c76e1fadb837c550153a` with the Python delivery feature applied in the working tree.
The private manifest records the exact input file hashes.
The native resolver and shared delivery implementation remain unchanged.

| Current contract or deployment gap | Deployment owner | Result |
| --- | --- | --- |
| `sandbox/process.rs::Config` admits preparation only for Python and requires dependency content. Kubernetes uses namespace egress policy. | `deploy/helm/elitea/values.yaml`, new `templates/sandbox/preparation-boundary.yaml` | An optional dedicated preparation namespace adds restricted Pod policy, bounded quota, resolver egress, and namespace-scoped Pod rights. |
| Existing `templates/sandbox/kubernetes-boundary.yaml` denies execution ingress and egress, including DNS. | Existing execution template, unchanged | Execution stays offline. Preparation requires a separate namespace and cannot select the platform or execution namespace. |
| Existing runtime Pods use `elitea-code`, no mounted token, fixed image digest, and `imagePullPolicy: Never`. | Optional preparation boundary and existing image warmer | The preparation namespace supplies the same unprivileged Pod account. Operators warm the exact image reference on selected nodes. |
| `sandbox/dependency_content.rs::validate_staging` requires an existing owner-private directory. The supervisor root is read-only. | `templates/sandbox/supervisor.yaml`, optional `contentStaging` values | Bounded memory volumes provide writable transfer storage. A pinned nonroot init image creates mode 0700 private roots. Material stays separate. |
| Existing certificate helper issues exact per-profile DNS server leaves and separate audience-matched content client leaves. | Optional `profiles[].serviceName` aliases in the consolidated supervisor template | Internal aliases map one certificate hostname to one listener. The existing supervisor Service remains available. |
| Existing projected material preparation copies one flat Secret revision into regular 0600 files. | Kubernetes preparation JSON example and deployment instructions | Operators add flat preparation and content keys within existing copier bounds. Runtime Pods receive no material Secret. |
| Main grants use `main.runtime.sandboxAudiences`; the Rust worker preserves `sandboxRuntimes` configuration. | Deployment instructions and worker render fixture | Add the exact preparation audience and Python-only optional object while preserving execution and workload identity configuration. |
| Existing focused sandbox render scripts are separate from runtime acceptance. | `render-sandbox-kubernetes.sh`, `render-worker-sandbox.sh`, `helm-lint.yml` | Render checks cover both namespace boundaries, scoped rights, staging, aliases, pinned images, invalid values, and optional worker object preservation. CI calls the Kubernetes script. |

Worker source paths are relative to `services/elitea-worker-rust`.
All deployment paths are relative to the repository root.
The shared supervisor remains the backend lifecycle owner. Main remains the receipt migration, authority, and durable content owner.

## Implementation history and compatibility

1. Freeze the current deployment files into private before/after copies.
2. Add optional preparation namespace values and a separate boundary template.
3. Add bounded content staging and an immutable shell init image to the existing supervisor template.
4. Add optional internal DNS aliases with unique sandbox names. Preserve the original Service and default mounts.
5. Add the Kubernetes preparation example, exact material instructions, and focused render cases.
6. Add one CI call for the existing Kubernetes render script.

The new values default to disabled preparation, no staging volumes, and no Service aliases.
The existing execution template, API rights, runtime Pod isolation, image warmer, phase clocks, and runtime source remain unchanged.
The preparation boundary has no ClusterRole, Secret rights, worker rights, ingress permission, or zero-prefix address grant.
The preparation Pod account has no token mount.

Kubernetes NetworkPolicy accepts IP CIDRs, not registry hostnames.
Operators supply approved registry or controlled gateway routes and verify DNS selectors, CNI enforcement, and destination limits.
Policies combine across the namespace. The chart cannot prove that another policy grants no additional routes.
Kubernetes memory volumes do not provide Docker's `noexec` tmpfs setting.
The staging directories remain private to the supervisor; no source path executes their contents.

This deployment change activates Python v1 only.
It introduces no JavaScript/TypeScript or Cargo preparation policy, native schema, launcher, or bundle format.
It does not change source admission, authority, indexed transfer, cancellation, recovery, or the accepted phase deadlines.

## Verification and acceptance limits

The private delivery records syntax checks and static source review only.
No Helm render, build, credential read, deployment, database access, image probe, or runtime test runs during assembly.

Application verifies the patch SHA-256, source branch, frozen base ancestry, all six input hashes, and patch applicability.
Both `render-sandbox-kubernetes.sh` and `render-worker-sandbox.sh` pass after application.
The local `task` executable is unavailable. The exact `helm:lint` task commands run directly instead.
All three chart lint commands pass. The NATS wrapper reports its existing missing dependency warning.
The four task prerequisites also pass: capability flags, LLM paths, edge health, and BF0.2b render checks.
These checks use fixtures. They do not verify live TLS material, registry routes, Pod execution, or browser behavior.

The parent task must verify both backend deployments through native publication, hydration, execution, Stop, and replacement.
Browser acceptance and complete Python delivery acceptance remain open until their required deployed cases pass.
See [Python delivery feature](code-python-delivery-feature-20261002.md) and [deployment instructions](../../../../deploy/runtime/sandbox-preparation-deployment.md).
