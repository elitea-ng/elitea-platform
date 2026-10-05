# Native dependency content helpers

Date: 2026-10-01.

The trusted runner adds binary dependency transfer and preparation release commands.
The helpers never execute user source or select a runtime.
The supervisor selects each fixed command and owns the transport deadline.

## Source mapping

| Current path or behavior | New helper | Evidence |
| --- | --- | --- |
| `../elitea-code-runner/src/lifecycle.rs::apply` prepares and dispatches execution jobs. | `run` routes four fixed content commands before the existing job input path. | The existing preparation and dispatch tests remain registered. |
| `lifecycle.rs::validate` fences Kubernetes operations by Pod UID and request digest. | `content_identity` reuses that validation for every content operation. | Tests reject replacement Pods, changed digests, omitted identity, partial identity, and explicit null fields. |
| `python_preparation_job.mjs` retains native files in `/workspace/python-dependencies`. | `dependency_read` streams one recorded file from that fixed directory. | Tests reject missing files, wrong lengths, symlinks, unsafe names, and trailing header input. |
| Python execution consumes frozen content from `/workspace/wheels`. | `dependency_write` publishes one complete file in that fixed directory. | Tests reject truncated input, excess input, changed existing content, and directory symlinks. |
| Preparation holding ends after a fixed empty release marker. | `preparation_release` publishes that empty marker with identity validation. | Tests verify empty publication, repeated release, replacement rejection, and invalid existing markers. |
| Docker archive copying does not retain the preparation tmpfs content in the parent probe. | Native helpers transfer bytes through the runtime exec stream. | The live Docker test exports from both fixed directory roles before computing host digests. |

The native helpers and their focused tests reside in `../elitea-code-runner/src/lifecycle.rs`.
The live transport test resides in `src/sandbox/dependency_runtime_docker_tests.rs`.
The change adds the pinned `tempfile` dependency for immutable publication and preserves the existing job preparation path.

## Fixed commands and framing

Call the image-owned runner with exactly one selected command:

```text
/usr/local/bin/elitea-code-runner --dependency-read
/usr/local/bin/elitea-code-runner --execution-dependency-read
/usr/local/bin/elitea-code-runner --dependency-write
/usr/local/bin/elitea-code-runner --preparation-release
```

The existing public entry point remains `lifecycle::run(command: &str) -> io::Result<()>`.
Private helper functions accept the workspace root, expected Kubernetes identity, and borrowed `Read` or `Write` streams.
The production workspace root remains `/workspace`.
No header field selects a directory, endpoint, executable, or runtime.

Read and write use one strict JSON header line on stdin:

```json
{"name":"elitea-python-lock.json","bytes":123}
```

Append one LF after the JSON object. Limit the complete header line, including LF, to 4096 bytes.
Use a flat ASCII filename of at most 256 bytes. Permit letters, digits, `.`, `_`, `+`, and `-` only.
Reject the special names `.` and `..`. Limit each file to exactly 32 MiB or less.
Reject unknown fields, duplicate fields, invalid numeric lengths, and missing LF.

For Kubernetes, add both `pod_uid` and `request_digest` to that same header.
Compare them with `ELITEA_SANDBOX_POD_UID` and `ELITEA_SANDBOX_REQUEST` inside the helper.
Reject partial identity fields, null fields, or identity fields without the trusted environment identity.
For Docker, omit both fields only when both trusted environment identity values are absent.
The Docker transport remains responsible for immutable container identity checks.

| Command | Stdin after the header | Stdout | Fixed file directory |
| --- | --- | --- | --- |
| `--dependency-read` | Immediate EOF. | Exactly the raw declared bytes. | `/workspace/python-dependencies` |
| `--execution-dependency-read` | Immediate EOF. | Exactly the raw declared bytes. | `/workspace/wheels` |
| `--dependency-write` | Exactly the declared raw bytes, then EOF. | Empty. | `/workspace/wheels` |

The transfer loop uses one 64 KiB buffer. Retry comparison uses two 64 KiB buffers.
Both loops accept short reads without retaining a complete file.
Read validates a regular source file and its exact length before output.
Linux and macOS reads use `O_NOFOLLOW`, `O_NONBLOCK`, and device/inode checks.
Unsupported read hosts fail instead of removing the no-follow requirement.
The transport must require a successful helper exit before accepting the streamed content.

Write creates a private temporary file with exclusive creation and mode `0600`.
Write rejects missing or trailing bytes before publication. Flush and sync the temporary file before publication.
The pinned `tempfile` dependency publishes with no replacement.
Linux and macOS use native no-replace rename when available. The library fallback publishes with an exclusive hard link.
Neither path exposes a partially written target or replaces an existing target.
An existing regular target accepts only an exact streamed byte match, including EOF.
Truncated, excess, different, or unsafe existing content fails without replacement.
After a publication race, compare the private spool with the existing target before accepting an identical retry.
Concurrent writes publish one complete immutable target. Both identical writers succeed without replacement.
Failed writes remove their own temporary files.

For Kubernetes release, send this strict bounded header line and immediate EOF:

```json
{"pod_uid":"original-pod-uid","request_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
```

For Docker release, send empty stdin and EOF when the trusted identity environment is absent.
Release atomically creates `/workspace/.elitea-python-preparation-release` as an empty regular file.
Repeated release accepts an existing empty regular marker. Release rejects symlink and non-empty markers.

All content-helper errors retain their safe I/O error kind and use one fixed diagnostic.
Headers and diagnostics contain no credentials or platform endpoints.
SHA256 verification remains in the shared bundle sender and receiver.
That verification binds the bytes to the recorded manifest. The helper does not accept a caller-selected digest as authority.

## Implementation history

1. Add strict bounded JSON line framing and fixed directory roles.
2. Preserve the existing Kubernetes Pod and request identity checks in the new commands.
3. Add exact binary streaming with a 64 KiB buffer and a 32 MiB file bound.
4. Add private temporary writes, no-replace publication, and fixed release marker creation.
5. Add exact streamed retry reconciliation without replacing immutable targets.
6. Add short-read, maximum-size, corruption, symlink, identity, and concurrent-write tests.

## Verification status for the extracted feature (2026-10-02)

This source is extracted from preserved work and reconciled with the current phase-deadline branch.
No stash-era test count, image identity, browser result, or runtime timing is evidence for this assembled patch.
The extraction runs formatting, source invariants, pinned protocol generation, protocol checks, shell/JSON syntax, and patch applicability only.
Rust builds, focused Rust tests, PostgreSQL tests, Docker and Kubernetes acceptance, and browser acceptance remain pending.
See [complete feature mapping and acceptance](code-python-delivery-feature-20261002.md) and [execution export correction](code-python-execution-export-20261002.md).
