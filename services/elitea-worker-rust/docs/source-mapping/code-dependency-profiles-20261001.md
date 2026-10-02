# Code dependency profile verification

This record extends the [Code functional audit](code-functional-parity-20260930.md).
Dependencies remain part of gate 5. The current SDK defines Python installation behavior.
JavaScript and TypeScript package imports extend that behavior through the selected Deno runtime.

## Source mapping

| Current source or behavior | New implementation | Ownership |
| --- | --- | --- |
| SDK `infra/data/sandbox/main.ts::find_imports_to_install` maps import names to distributions. | `services/elitea-code-runner/adapters/python.mjs` uses Pyodide's frozen import-name mapping. | The immutable runtime image supplies package content. Each invocation receives private writable wheel storage. |
| SDK `main.ts::install_imports` installs missing packages with micropip. | `adapters/preload.mjs` resolves approved exact requirements and freezes their installed dependency closure before image publication. | The execution container does not download packages. Inline micropip calls use the prepared closure. |
| JavaScript and TypeScript require package imports in the new Code contract. No equivalent SDK package contract is assumed. | `adapters/prepare_javascript.mjs`, `javascript-packages.json`, and `Containerfile` prepare the native Deno npm graph. | The operator supplies exact requirements. Deno records resolved transitive versions and package integrity. Preparation does not execute imported modules. |
| SDK Code execution receives selected state. | Rust `services/elitea-code-runner/src/execute.rs::command` selects the fixed adapter and disables configuration discovery and mutable module installation. | The container owns execution limits. Rust `agents/graph/code_state.rs` owns typed state publication. |
| Retried jobs must retain dependencies. No legacy crash-recovery behavior is assumed. | Rust `sandbox/request.rs::PreparedJob` includes image and policy identity in its immutable request fingerprint. | The supervisor owns dispatch and receipts. The worker owns graph checkpoints. |

The default Python and npm package lists remain empty.
Approved image profiles can add exact dependencies without granting runtime network access.
The npm build uses Deno's native cache and lockfile. It does not enable lifecycle scripts or native addons.
Runtime resolution remains frozen and cached-only. FFI and subprocess permissions remain denied.
On-demand acquisition, private registries, Cargo profile expansion, and compiled-artifact caching remain open.
Build-system resource and network policies govern native npm preparation. This change does not impose per-download limits on that resolver.

## Focused and Docker evidence

- Two npm profile tests reject ranges, tags, foreign sources, injected source, trailing newlines, and oversized profiles.
- Three offline npm execution tests verify JavaScript, TypeScript, and rejection of an unprepared version.
- The prepared npm profile includes `strip-ansi@7.1.0`, `slugify@1.6.6`, and `csv-parse@5.6.0`.
- It exercises a transitive dependency, CommonJS interoperability, and a package export subpath.
- Eight Rust runner tests, Clippy, formatting, and adapter lint pass.
- All thirteen real Docker adapter checks pass against the combined Python/npm image.
- Containers use UID 10001, read-only root, disabled network, one CPU, 512 MiB memory, and a bounded private workspace.
- Checks verify successful results, explicit failures, denied network and subprocess access, timeout, and identical repeated terminal receipts.

The Docker image identity is `sha256:de4c697471c0020d3027094dc1260a3f981433fa3f76f8b7d1f501b2eac09e30`.
The imported Kubernetes manifest identity is `sha256:c242f802a226a5429e154de9bbad8ddede27cc16a686e82963f37190eb2aaa11`.
Use the immutable identity accepted by each backend. Do not infer Kubernetes manifest identity from Docker's local image identifier.
Both profile configurations and worker runtime references must use the same backend image identity.

## Browser and deployment evidence

Pipeline 139, version 146, contains automatic-import and inline-install Python nodes.
Persistent chat 767 verifies `python-slugify==8.0.4` and its `text-unidecode==1.3` dependency.
Both nodes return the expected normalized text. Browser reload preserves the result.
A second message starts fresh isolated jobs and returns the same result without runtime downloads.
The first attempt cannot start because its imported Kubernetes digest reference is absent.
Correcting the manifest reference permits regeneration to complete. The source and package content are unchanged.
All completed test Pods are removed after settlement.

Pipeline 140, version 147, transfers parsed CSV state between JavaScript and TypeScript Code nodes.
The ephemeral editor chat returns the expected result through the deployed Kubernetes supervisor.
Persistent chat 769 returns the same result: two rows, total five, normalized text, and both language markers.
Browser reload preserves that exact result. The browser reports no console errors for this check.
The prepared profile runs without execution-container network access. Terminal execution Pods are removed after settlement.
Gate 5 remains open for on-demand dependencies and the other functional boundaries in the Code audit.

Native reference: [Deno npm compatibility](https://docs.deno.com/runtime/fundamentals/node/).
