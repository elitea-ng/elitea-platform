# Code platform client integration

The runner retains one original Code child and verifies each committed Main reply before pipe delivery.
The supervisor retains admission, runtime identity, cancellation, and recovery ownership.
The container runtime enforces process, resource, filesystem, and network isolation.

## Source mapping

| Current behavior | Integrated behavior | Owning source |
| --- | --- | --- |
| Direct Deno execution | Retained child pipes and signed replies | `src/execute.rs`, `src/code_platform_bridge.rs`, `src/code_platform_signature.rs` |
| Legacy Rust wrapper | Exact broker profile selection and fixed image modules | `src/rust_execute.rs`, `src/rust_profile_selector.rs` |
| Cargo archive retains main and user source | Archive also retains three exact broker modules | `src/rust_prepare_archive.rs` |
| Async Python execution | Pyodide installs the fixed client module and awaits retained pipe calls | `adapters/python.mjs`, `adapters/platform_client.py` |
| JavaScript and TypeScript execution | Both languages await the injected client and restore prior globals | `adapters/javascript.mjs`, `adapters/platform_client.mjs` |
| Structured result output | The complete envelope has a 256 KiB limit and exclusive private publication | `adapters/platform_prepared.mjs`, `adapters/rust/src/main_platform.rs` |
| Workspace hydration | Native entry verifies the exact manifest and read-only kernel mount before Code starts | `src/workspace_lifecycle.rs` |

## Contracts

Broker requests use revision 5 and platform capability revision 1.
Compiled launch revision 2 retains the original Execute digest separately from the prepared request fingerprint.
The trusted Main public-key asset uses the fixed image path `/opt/elitea-code-trust/main-receipt-keys.json`.
The operator supplies this asset during image composition.
This source integration does not supply keys or enable production admission.

The static broker profile uses the exact template and Cargo-generated `adapters/rust/platform-v1.Cargo.lock`.
The image build generates its vendor configuration with locked Cargo dependencies.
The archive accepts only three additional fixed module paths.
Unknown source paths, substituted modules, symlinks, and replay mismatches still fail.

## Verification boundary

Focused native Cargo tests compile the composed runner and fixed broker wrapper on macOS.
Deno tests execute JavaScript, TypeScript, and cached Pyodide without network access.
The Python broker fixture uses the original adapter and retained pipe implementation with in-memory Main pipes.
These checks do not prove Docker, Kubernetes, Main persistence, Stop, or recovery behavior.
The root integration owns measured image validation, public-key installation, and live acceptance.

Keep the worker implementation history in `services/elitea-worker-rust/docs/source-mapping/code-node-isolation-assessment-20260928.md`.
