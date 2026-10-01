# Python on-demand preparation component

Date: 2026-10-01. Gate 5 remains active.

This change implements dependency discovery, native resolution, frozen content, and verified reuse.
The worker and supervisor do not call this component yet.
The deployed pipeline still uses operator-built image profiles.
This component does not close on-demand installation acceptance.

## Source mapping

| Current source and behavior | New source | Result |
| --- | --- | --- |
| SDK `infra/data/sandbox/main.ts::find_imports_to_install` maps imports to package names. | `services/elitea-code-runner/adapters/python_requirements.mjs::discoverPythonRequirements` | Native Pyodide import mappings select distributions. Standard-library imports need no package. |
| SDK `main.ts::install_imports` installs missing dependencies through micropip. | `adapters/preload.mjs::preparePythonPackages` | Native micropip resolves dependencies. `micropip.freeze()` retains their exact versions and integrity. |
| SDK explicit micropip source controls installation order. | `python_requirements.mjs` uses Python AST inspection. | Literal calls retain source order. Preparation does not evaluate user expressions or execute user source. |
| SDK runtime installation downloads package content during code execution. | `adapters/prepare_python_code.mjs::preparePythonCodePackages` | A separate preparation operation acquires content before offline execution. Production dispatch integration remains open. |
| No legacy dependency recovery contract is assumed. | `prepare_python_code.mjs::verifyPythonCodePackages` | A caller supplies the previously recorded content digest. Reuse verifies each file without native resolution or downloads. |
| SDK `main.ts` contains an implicit `chardet<6` compatibility override. | Native requirement resolution and explicit literal requirements | This change does not copy a global package override. Callers can declare a compatible version constraint. |

Python AST parsing, native import discovery, native requirement parsing, and micropip resolution define package behavior.
No replacement dependency solver is added.
ADK workspace manifests remain the provision boundary for execution files.
Their current deployment adapters do not provide a complete dependency store or preparation lifecycle.

## Component contract

Discovery accepts at most 256 KiB of source and 40,000 AST nodes.
It accepts at most 128 requirements, with at most 256 characters per requirement.
Literal strings, lists, tuples, and direct micropip import aliases are supported.
Dynamic expressions, custom installer options, custom indexes, direct URLs, and environment markers remain unsupported.
Dynamic expressions fail with guidance to use literal requirements.
Discovery conservatively includes literal calls inside functions and conditional branches.
It does not decide which user branch will run.

Operator-built image profiles still require exact versions by default.
On-demand preparation accepts native package names and version constraints.
The resolved lock contains exact versions even when the requested requirement has no exact version.
Preparation keeps the native lock format and the installed dependency closure.
It does not execute the supplied Python source.

A bundle records the native runtime revision, requested requirements, file names, lengths, and SHA-256 values.
Its root digest covers that complete record.
The lock has a 1 MiB bound. Bundle metadata has a 128 KiB bound.
Each wheel has a 32 MiB bound. The bundle has 257 files and 128 MiB limits.
Bounded file reads reject links, changed file identity, truncated content, and growth during reading.
Root verification rejects changed metadata, content, unsafe paths, and a different recorded digest.
Callers must place content in a private immutable location before execution.
Verification is not an authorization grant or an atomic publication operation.

An existing bundle cannot be prepared again in place.
The `--verify` command requires the independently recorded digest.
It does not take the expected digest from untrusted cache metadata.
Registry resolution does not run during verified reuse.
The execution adapter installs from the frozen closure with network access disabled.

## Verification

Thirteen focused Linux-container checks cover discovery, default policy, source restrictions, integrity, file bounds, and immutable reuse.
Two execution checks verify automatic imports and inline version constraints against the resolved closure.
Formatting and lint pass for all seven changed adapter modules.

Trusted local preparation acquires `humanize==4.13.0`, which is absent from the benchmark image.
It separately resolves `humanize>=4.13,<4.14` to version 4.13.0.
Automatic import discovery resolves `humanize` to version 4.16.0 during this test.
The automatic source contains a deliberate exception. Preparation succeeds without executing that source.
Its frozen bundle also passes the offline automatic-import execution check.

Each offline execution processes 20,000 values and returns this exact result:

```json
{"records":20000,"total":1001850000,"formatted":"1,001,850,000","size":"20.0 kB","status":"PASS"}
```

The exact-version bundle digest is `940294b8e6e8265fa753191d42774246c7fe0670317819e61d9db8eb3360d2d6`.
The range bundle digest is `71d60b3a4bcbef14d4007fe9339ec6f7cff9c4a4c1a320f0e9c07ca26cd5d625`.
The automatic-import bundle digest is `4a607bef5d20a9d3c0ec523aaae1548c1fa1414e60c285572acc387d5d76d5dd`.
These digests identify different requests and their resolved content. They are not deployment grants.

The Docker test image is `sha256:40fc267539dca68c03ae56182d9b96585aa15dd640e3e64c9d580fe0f537584a`.
Execution uses UID 10001, one CPU, 512 MiB memory, disabled network, and a read-only root.
The package mount is read-only. The writable workspace has a 256 MiB bound.
Subprocess and FFI access remain disabled.

Symlink fixtures require unscoped Deno file permissions in the trusted integrity test process.
The container still enforces its read-only image and package mounts.
The execution checks retain path-scoped Deno permissions. Production runtime permissions are unchanged.

The first direct execution probe fails because its fixture assumes a native `/workspace` path inside Pyodide.
The corrected fixture creates that directory in Pyodide's virtual filesystem and returns the exact result.
This is a fixture correction, not a runtime filesystem permission change.

## Open dispatch contract

Persist the resolved bundle identity before dispatch.
Bind the runtime image, operator package policy, dependency bundle, and admitted source to the durable execution.
Retain the same lock and content digest after worker or supervisor replacement.
Never resolve an admitted job again against current registry metadata.

Use the existing Main object-store contract for shared content where practical.
Keep package bytes outside the 1 MiB sandbox control request.
The Kubernetes adapter currently accepts only the code and runner manifest files.
It needs bounded dependency delivery before it can consume this bundle.
The image launcher also needs an explicit dependency input before execution.
Do not let its default image-asset copy overwrite the admitted lock.
The [bundle admission consumer](code-python-bundle-admission-20261001.md) now implements that request identity and launcher verification.
Automatic production delivery remains open.

Implement bounded acquisition, cancellation, atomic publication, and durable preparation receipts in the owning supervisor path.
The materializer bounds wheel content. The preceding native resolver still needs process, metadata, and time limits.
Prove authorized registry access, immutable shared-content reuse, and project isolation through the complete deployment.
Complete persistent-chat and ephemeral-editor acceptance on Docker and Kubernetes.
Package-not-found and incompatible-wheel failures must stop downstream pipeline nodes with useful messages.

JavaScript/TypeScript on-demand resolution, user-selected Cargo dependencies, and compilation caching remain separate open contracts.
Platform-client access, workspace access, debug exports, and variable-source admission remain in the Code functional audit.
No application database migration, control-protocol change, or deployed runtime update is made by this component.

Native references: [Pyodide package loading](https://pyodide.org/en/0.29.0/usage/loading-packages.html),
[micropip API](https://micropip.pyodide.org/en/stable/project/api.html),
and [Deno filesystem permissions](https://docs.deno.com/api/deno/file-system/#function-denosymlink).
