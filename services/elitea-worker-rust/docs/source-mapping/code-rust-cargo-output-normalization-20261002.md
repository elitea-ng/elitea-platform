# Rust Cargo output capture correction

## Source mapping and ownership

The SDK Code function remains the behavior reference for selected state and declared outputs.
The SDK has no compiled executable publication authority.
The owning history is `code-node-isolation-assessment-20260928.md`.
The runner changes only the local compiler output capture boundary.

| Existing source | Corrected source | Preserved behavior |
| --- | --- | --- |
| `services/elitea-code-runner/src/rust_execute.rs` compiler completion and descendant drain | Unchanged | Capture starts after the owned compiler and its descendants stop. |
| `compiled_snapshot.rs::capture` | `read_compiler_output` | Only fixed Cargo output receives the special link check. |
| `compiled_code.rs::regular_file` and `read_regular` | Unchanged | All general readers reject multiple hard links. |
| `compiled_code.rs::immutable_file` | Unchanged | Publication creates a fresh single-link executable with mode `0500`. |
| `compiled_snapshot.rs::cached_program` and descriptor verification | Unchanged | Fresh execution requires the selected immutable descriptor and readiness proof. |

## Observed failure

The isolated Linux arm64 probe uses image `sha256:015c96d0aeb233aaee112659e346340fd204195ad7b9f0bf574327c3c106e990`.
Cargo produces two paths for one executable inode.
The paths are `target/debug/elitea-code-job` and `target/debug/deps/elitea_code_job-e19751f0b3163378`.
Both paths have inode `211`, link count `2`, length `989456`, and mode `0755`.
The unchanged capture reader rejects link count `2`.
Toolchain, profile, source, request, and launch binding checks already pass in this probe.

## Capture contract

Single-link output retains the existing general reader.
Two-link output must use the fixed binary name beneath a `debug` directory.
Its second path must be one regular, inode-identical `deps/elitea_code_job-<16 lowercase hexadecimal digits>` file.
The reader rejects other link counts and missing, unknown, or symbolic aliases.
The source owner must match the workspace owner.
The directory scan has a limit of `16384` entries.
The executable retains its `32 MiB` byte limit.
The reader opens the source with `O_NOFOLLOW` and `O_NONBLOCK`.
It checks device, inode, length, links, owner, mode, and modification timestamps before and after reading.
It checks both source paths again after reading.
Publication copies the verified bytes into a new immutable single-link file.
A later write through the old Cargo alias cannot change published bytes.

The special reader is private to capture after compiler descendant drain.
It grants no new caller path, process, content, network, or runtime authority.
General import, storage, publication, and cached execution checks stay unchanged.
The descriptor format and snapshot key stay unchanged.

## Verification boundaries

Five new unit tests cover alias isolation, metadata changes, unknown names, extra links, symlinks, and size rejection.
The existing offline cold/warm component fixture now builds the fixed production binary name.
Root's assembled runner passes 71 locked offline tests and strict Clippy.
Two existing network preparation tests remain ignored.
The shipping Linux arm64 runtime builds as `sha256:8c403798c060330cf9836a583ea5d803890b91387b0088f3e64f005c22b00024`.
The restricted Linux cold/warm probe passes against that image.
Cold capture publishes verified bytes after compiler descendants stop.
Warm execution uses a distinct sandbox and performs zero compilation calls or user build-script calls.

Five image-matched Linux tests prove descendant cleanup, deadline cleanup, owner-drop cleanup, and overlapping-finalization rejection.
The probe also rejects compiler export from the Execute role and preserves inert import before dispatch.
Failure, timeout, and cancellation publish no executable.
Owned probe containers are removed afterward.
The retained private receipt is `compiled-runner-probe/run-06/results.json` under the root acceptance directory.

The synthetic probe does not prove Main-issued grant authority or product deployment behavior.
Native runtime assets still require exact indexed hydration before warm dispatch.
Generated companion artifacts remain a separate cache eligibility gate.
