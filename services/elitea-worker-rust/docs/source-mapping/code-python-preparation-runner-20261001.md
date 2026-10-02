# Retained Python preparation adapter

Date: 2026-10-01.

The adapter retains native dependency files while the supervisor publishes their content.
This change adds a trusted image component. It does not dispatch preparation or execute user source.

## Source mapping

| Current behavior or contract | New adapter path | Evidence |
| --- | --- | --- |
| SDK `infra/data/sandbox/main.ts::install_imports` maps imports to distributions. | `python_requirements.mjs::discoverPythonRequirements` remains the native syntax planner. | The adapter passes the exact source to the existing preparer. |
| `sandbox/preparation.rs::PreparationJob` defines revision 1 preparation transport. | `python_preparation_job.mjs::decodePreparationJob` checks the same field shape and bounds. | Tests reject duplicates, unknown fields, invalid identities, and execution fields. |
| `prepare_python_code.mjs::preparePythonCodePackages` resolves and freezes native package content. | `runPythonPreparation` calls this existing preparer in a new private directory. | Tests verify exact source delivery and marker absence before native verification. |
| `prepare_python_code.mjs::verifyPythonCodePackages` verifies recorded content without registry resolution. | New preparation and marker reuse call the existing verifier. | Tests reject verification failures and changed recorded bundle metadata. |
| Container temporary files remain available while the container runs. | The adapter holds after atomic marker publication until release or the original deadline. | An injected wait proves that publication does not complete the adapter process. |
| Retry and recovery must retain resolved package identity. | Matching markers reuse native bytes and retain their first deadline. | Tests prove that restart does not call preparation or renew the deadline. |

The SDK path is a behavior reference. The adapter reuses the current native planner and preparer.
The adapter adds no package resolver, installer, credential lookup, grant handling, or platform client.

## Fixed contract

The image path is `/opt/elitea-code/python_preparation_job.mjs`.
The script accepts no CLI arguments.
The supervisor owns the complete Deno argv, permissions, admitted image, registry allowlist, and outer deadline.
The image preloads `/opt/elitea-code/prepare_python_code.mjs` and its module dependencies.
The adapter loads that module inside the preparation deadline.

| Fixed path | Purpose |
| --- | --- |
| `/workspace/.elitea-code.json` | Bounded strict preparation request. |
| `/workspace/python-dependencies` | Private native lock, wheels, and bundle files. |
| `/workspace/.elitea-python-preparation.json` | Atomic bounded success marker. |
| `/workspace/.elitea-python-preparation-release` | Empty regular release marker. |

The success marker has revision `1` and status `resolved`.
Its fields are `source_sha256`, `preparer_image_digest`, `policy_revision`, `timeout_seconds`, `started_unix_ms`, `deadline_unix_ms`, and `bundle`.
The `bundle` field retains the verified native bundle metadata and content digest.
The source, execution input, credentials, endpoints, and grants do not enter the marker.
Successful preparation emits no stdout envelope. The supervisor reads the marker while the process holds.
Failures emit bounded JSON with a fixed safe error code on stderr.
The adapter exits with code `124` when its deadline expires. Other failures use code `1`.

The adapter rejects partial directories, stale temporary markers, unsafe files, and unknown bundle metadata.
Marker reuse requires matching source, image, policy, timeout, original deadline, and verified native content.
The adapter uses a monotonic clock within each process. It records the original wall deadline for restart checks.
The outer container-main-process runner remains responsible for process termination and descendant cleanup.

## Implementation history

1. Add strict bounded revision 1 decoding with duplicate rejection and literal source preservation.
2. Add private native preparation and verification with atomic success publication.
3. Add process holding, explicit release, and one deadline across preparation and holding.
4. Add verified reuse with the original deadline and rejection of partial resolution.
5. Add safe fixed errors, native diagnostic suppression, and injected component tests.

## Verification and remaining checks

Run these checks from `services/elitea-code-runner`:

```sh
deno test --cached-only --no-lock --no-config --no-prompt --deny-net \
  --allow-read --allow-write adapters/python_preparation_job_test.mjs
deno lint adapters/python_preparation_job.mjs adapters/python_preparation_job_test.mjs
deno check --no-lock --no-config adapters/python_preparation_job.mjs adapters/python_preparation_job_test.mjs
```

The focused suite passes 17 tests with no skipped tests.
The Deno lint and module checks pass.
The component tests inject preparation, verification, clocks, and waits.
They do not perform registry resolution or validate Linux process isolation.

The native Linux container probe resolves four files in 3.36 seconds and retains them during shared publication.
The immutable Deno image runs as UID 10001 with bounded CPU, memory, PIDs, and tmpfs.
The probe mounts the new trusted adapter read-only; the production image does not yet contain this adapter.
The supplied Python source ends with a deliberate exception and is never executed.
Binary export through the running container verifies each file length and SHA-256.
The real Main content handler and Rust client publish these files to rehearsal RustFS through verified mTLS.
The probe creates the release marker only after publication succeeds.
The container-main-process runner then returns exit code zero.
The probe removes its containers and test objects.

One local observation reports 1.03% CPU and 269.9 MiB memory while holding.
This observation is not a capacity benchmark.
Docker archive copy initially cannot read the mounted tmpfs file in this probe.
The corrected probe exports bytes through the running container and verifies the recorded file identity.
Image wiring, fixed dispatch, content publication, durable receipts, worker replacement, and deployment acceptance remain integration checks.
This source mapping does not claim those checks pass.
