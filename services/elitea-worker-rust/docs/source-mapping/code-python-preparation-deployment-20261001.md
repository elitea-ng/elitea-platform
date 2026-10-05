# Optional Python preparation deployment

Date: 2026-10-01.

This change adds an optional Docker overlay for the existing consolidated supervisor.
It provides example configuration and separate TLS leaves for Python preparation and content delivery.
It does not change the base Compose files, current worker configuration, or live deployment.

## Source mapping

The SDK behavior reference uses revision `a54db410a46bac5e2c3cbc2db46c148a5c3d069c`.

| Current source and behavior | New source | Result |
| --- | --- | --- |
| SDK `infra/data/sandbox/main.ts::install_imports` installs missing Python dependencies before source execution. | `deploy/docker-compose.sandbox-preparation.yml` | An optional preparation listener runs in the existing supervisor process. Separate content authority supports publication and execution downloads. |
| Existing supervisor composition supports multiple bounded listeners in one process. | `deploy/runtime/sandbox-preparation.example.json` | The third profile uses preparation purpose, Python only, a distinct owner, port 9448, and an explicit resolver network. |
| Current `services/elitea-worker-rust/src/sandbox/process.rs::ContentConfig` selects supervisor content TLS and private staging. | Preparation example and Deno additive configuration in `sandbox-preparation-deployment.md` | Separate client leaves match each profile audience. Two bounded private tmpfs mounts hold disposable staging. |
| Current `services/elitea-worker-rust/src/config.rs::PythonPreparationConfig` is optional on the Python runtime. | Worker example in `sandbox-preparation-deployment.md` | Operators enable preparation without replacing the current worker identity or unrelated configuration. |
| Existing `gen-sandbox-certs.sh` issues Deno, Rust, and PostgreSQL server leaves under the current runtime CA. | Optional `--preparation` argument | New preparation server and content client leaves use separate keys. Existing files and the default issuance behavior remain unchanged. |
| Existing Main sandbox grants allow configured exact audiences. | Optional Compose audience list | Main includes `dns:elitea-sandbox-preparation` alongside the existing execution audiences. |
| Existing Docker execution profiles use network `none`. | Operator-selected preparation network in the example | Trusted native resolution uses a dedicated network. Execution profiles retain offline operation. |

## Implementation history and limits

The existing sandbox overlay starts Deno and Rust profiles in one nonroot supervisor.
The optional overlay extends its command with a third configuration path.
It preserves the base listener paths and adds the preparation DNS alias.
The supervisor keeps its existing capability, process, CPU, and read-only root settings.
Its optional memory limit is 1 GiB. Each content staging tmpfs has a 256 MiB limit.

The preparation example bounds native resolution with one job, one CPU, 512 MiB memory, and a 120-second timeout.
Both example content clients use capacity one and a 30-second deadline.
The configuration binds an immutable preparer image and operator policy.
The named resolver network is an operator prerequisite.
Compose does not create that network or enforce its registry destination policy.
The operator must verify DNS and egress restrictions before activation.

The optional certificate flag issues one serverAuth preparation leaf and two clientAuth content leaves.
Each new leaf has one DNS SAN and one extended key usage.
The certificate helper verifies existing leaves and rejects additional identities or combined purposes.
The helper preserves existing keys and certificates instead of rotating them.
It does not install material into running containers.

The deployment instructions add dependency content configuration to the private Deno execution profile.
They add preparation configuration only to the private Python worker profile.
They do not alter tracked current runtime JSON or local secret files.

## Verification status for the extracted feature (2026-10-02)

This source is extracted from preserved work and reconciled with the current phase-deadline branch.
No stash-era test count, image identity, browser result, or runtime timing is evidence for this assembled patch.
The extraction runs formatting, source invariants, pinned protocol generation, protocol checks, shell/JSON syntax, and patch applicability only.
Rust builds, focused Rust tests, PostgreSQL tests, Docker and Kubernetes acceptance, and browser acceptance remain pending.
See [complete feature mapping and acceptance](code-python-delivery-feature-20261002.md) and [execution export correction](code-python-execution-export-20261002.md).
