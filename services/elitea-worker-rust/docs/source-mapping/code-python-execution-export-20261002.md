# Python execution dependency export

Date: 2026-10-02.

This change separates preparation export from execution export.
Preparation files stay in `/workspace/python-dependencies`.
Imported execution files stay in `/workspace/wheels`.
Final hydration and submission verify the imported execution files before dispatch.

## Source mapping

The current SDK package behavior remains mapped in `code-python-demand-preparation-20261001.md`.
SDK `infra/data/sandbox/main.ts::install_imports` prepares packages before Python source execution.
This correction changes the Rust delivery boundary. It does not change native package discovery or resolution.

| Previous source and behavior | Corrected source and behavior |
| --- | --- |
| Runner `lifecycle.rs::dependency_read` reads the fixed preparation directory. | It retains that role. New `execution_dependency_read` reads the fixed execution directory. |
| Runtime `export_dependency` serves preparation publication. | New `export_execution_dependency` serves final hydration and submission verification. |
| `docker_dependency_delivery.rs` verifies execution files through preparation export. | Both execution proofs use `export_execution_dependency`. |
| Docker `export_code_dependency` invokes `--dependency-read`. | New `export_code_execution_dependency` invokes `--execution-dependency-read`. |
| Kubernetes content export invokes `--dependency-read` under exact Pod identity. | New execution export uses `--execution-dependency-read` under the same identity checks. |
| The Docker transfer test checks destination files with a separate checksum command. | It exports destination bytes through the execution helper and checks their digest on the host. |

Worker paths are relative to `services/elitea-worker-rust`.
The runner path is `services/elitea-code-runner/src/lifecycle.rs`.

## Fixed roles and authority

The new command accepts the existing bounded filename and byte-count header.
The header cannot select a directory, executable, endpoint, or runtime.
The read implementation retains regular-file, length, framing, and no-follow checks.
Both commands reject missing files in their selected directory.
Neither command falls back to the other directory.
Imported files retain immutable publication and exact retry comparison.

Docker retains original-instance checks before and after transfer.
Kubernetes retains original Pod UID and request-digest checks in the adapter and helper.
The supervisor retains separate execution and content authority checks.
Final hydration verifies all imported files before it imports metadata.
Submission verifies the imported metadata before durable dispatch.
Preparation publication continues to export preparation files.
Phase timestamps and deadline policies are unchanged.

## Implementation history

1. Identify mismatched preparation and execution export directories in the preserved integration stash.
2. Add one fixed execution-read command through the existing binary transport.
3. Add distinct runtime methods and route both execution proofs through them.
4. Add native role and identity regressions.
5. Replace separate destination checksum commands in the Docker transfer test.
6. Add final hydration and submission coverage with a role-aware runtime fixture.

## Verification limits

This patch is prepared outside the shared worktree.
Formatting and patch application checks do not prove compilation or runtime behavior.
No Cargo build, Docker run, database test, deployment, or browser test runs during patch preparation.

The native tests verify fixed roles, binary bytes, missing-file refusal, identity checks, symlinks, and framing.
The opt-in Docker test verifies the execution helper against real imported content and replacement fencing.
The PostgreSQL component test calls the public final Hydrate and Submit operations.
It uses signed execution and content grants with a runtime fixture.
It refuses corrupted imported content and changed metadata before dispatch.
It verifies inert readiness, one dispatch, and terminal replay.
It does not download content or execute Python source.

Run the native lifecycle tests and the existing opted-in Docker transfer test after rebuilding the runner image.
Run `final_hydration_and_submission_verify_execution_files_before_dispatch` with `ELITEA_TEST_DATABASE_URL` and `ELITEA_TEST_BUNDLE_TLS`.
The TLS directory must contain test-only `ca.pem` and `client-combined.pem`.
The final-index component test performs no HTTP request.

Complete Main publication, indexed download, final hydration, and real offline Python execution through the deployed worker and supervisor.
Verify persistent chat, the pipeline test interface, replacement recovery, Stop, and missing-package failure.
Run Kubernetes acceptance separately before claiming that backend works.
