# Four-language data-processing fixture

This fixture processes 20,000 deterministic transactions through four Code nodes.
It uses actual language runtimes. It does not invoke a model.
The fixture verifies packages, state transfer, exact results, and single-run performance.
It does not prove the platform's concurrent-user capacity.

## Processing stages

| Language | Work | Language features | Packages |
| --- | --- | --- | --- |
| Python | Generate transactions, normalize names and dates, sort records, and encode CSV. | Dataclass, factory class, property, async batches, and `asyncio.gather`. | `python-slugify`, `python-dateutil`, and their dependencies. |
| JavaScript | Parse CSV, validate tuples, restore identity order, and aggregate categories. | Functions and an async entry point. | `csv-parse`, `zod`, and `fflate`. |
| TypeScript | Validate typed records, reconcile counts, and calculate SHA-256. | Types, interface, class, async batching, and Web Crypto. | `zod` and `fflate`. |
| Rust | Decode compressed state and reconcile independent category ledgers. | Deserialize struct, trait, implementation, async functions, and `join_all`. | `serde`, `base64`, `flate2`, and `futures`. |

The async helpers run within one bounded execution process.
They do not introduce threads or increase the CPU quota.
Compressed intermediate state stays within the existing result limit.
Artifact-backed large input remains a separate implementation requirement.

## Prepare immutable images

Copy the code-runner directory into a private build directory. Exclude `target`.
Copy this fixture's Python and npm profiles into that directory's `adapters` folder.
Copy `Cargo.toml` and `Cargo.lock` into its `adapters/rust` folder.
Build the `deno-runtime` and `rust-runtime` targets with `Containerfile`.
Read each canonical image ID with `docker image inspect`.

These profiles are test inputs. The default approved profiles remain unchanged.
Python's inline `micropip.install` uses the prepared offline closure.
This test does not implement arbitrary on-demand downloads.
Package preparation occurs before execution. It is excluded from processing timing.
Rust compiles its prepared dependencies and user source on every invocation.

Run the isolated Docker probe:

```sh
python3 scripts/runtime/probe_code_data_processing.py \
  --deno-image sha256:<canonical-deno-id> \
  --rust-image sha256:<canonical-rust-id> \
  --runs 3 --report /tmp/code-data-processing-report.json
```

The probe creates only test-owned containers. It removes each container after its receipt check.
Each container uses UID 10001, one CPU, 512 MiB memory, and a 256 MiB workspace.
The root filesystem is read-only. Network access and additional capabilities are disabled.
The probe compares all deterministic output fields with `expected.json`.
It verifies unchanged repeated receipts and completed process termination.

`processing_ms` measures work inside the user program, after runtime startup.
`sandbox_ms` uses Docker's start and finish timestamps. It includes Rust compilation.
`harness_wall_ms` includes sequential container creation, result reads, verification, and cleanup.
The first run uses existing local images. It is not a fresh image pull.
Later runs start new isolated processes. They do not reuse Python or compiled Rust processes.

## Verify through the UI

Deploy the prepared image identities through the supervisor and worker runtime profiles.
Create a test pipeline through the UI with `pipeline.yaml`.
Send `input.json` through the editor Test chat and a persistent participant chat.
Compare the final JSON with `expected.json`. Ignore variable timing fields during comparison.
Reload the persistent chat. Verify one unchanged final result.
Verify four completed supervisor receipts and execution-resource cleanup for each run.
Repeat this fixture after the artifact path is implemented.
