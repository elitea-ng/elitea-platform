# Compiled runner launch binding

The compiled runner requires the exact trusted image digest and policy revision in its original PID 1 environment.
This slice supplies both values in Docker and Kubernetes.
The supervisor selects the policy revision from its configured admission policy after it verifies the snapshot profile and prepared job.
Each backend selects the image digest from its own pinned runtime image.
Code source, request input, and environment overrides cannot select these values.

## Source mapping

| Existing source | Changed source | Behavior |
| --- | --- | --- |
| `DockerSupervisor::validate_snapshot` | Existing checks remain unchanged | Verify Main-attested profile, exact job, configured image, policy, timeout, and platform. |
| `DockerSupervisor::provision_snapshot` | `snapshot_launch_policy` and `verify_snapshot_launch` | Pass configured policy and verify original launch before readiness or dispatch. |
| `CodeJobRuntime::prepare_compiled` | Add trusted policy revision argument | Keep the existing fixed role and control hash contract. |
| Docker `provision_compiled_code_job` | Existing compiled content module | Add image digest and configured policy to the original PID 1 environment. |
| Kubernetes `create_with_compiled_launch` | Existing Kubernetes client | Add both values and verify the created or conflicting original Pod. |
| Existing compiled observation and publication | Original launch validation | Reject altered launch values before reading, exporting, releasing, or signalling a compiled runtime. |
| Existing compiled transfers | Backend launch validation | Bind every helper operation to the original role, control hash, image, policy, and runtime. |

## Recovery boundary

Require exactly one direct value for each compiled launch variable.
Reject missing, duplicate, indirect, or changed values.
Verify the original Docker container ID, image ID, job label, and request label.
Resolve the configured pinned Docker image locally without registry access.
Verify the original Kubernetes UID, configured image, job annotation, and request annotation.
Reject same-name replacements.
Do not mutate an original launch or create a replacement during validation.

Keep completed durable receipts recoverable after confirmed runtime cleanup.
Do not require a removed runtime to reappear for that receipt path.
Keep constructors disabled until the verified snapshot profile is supplied.
Do not change snapshot keys, grant roles, claims, prepared bytes, or public transport.

## Verification boundary

Run focused launch, conflict, missing-value, duplicate-value, altered-value, and original-runtime tests.
Use an in-memory Kubernetes API and typed Docker inspection fixtures.
These tests do not prove Docker, Kubernetes, PostgreSQL, mTLS, or process restart behavior.
Root owns assembled strict lint checks and live runtime acceptance.

## Implementation history

- 2026-10-02: Capture the exact cold hydration postimage before launch changes.
- 2026-10-02: Add trusted image and policy launch values in both deployment backends.
- 2026-10-02: Verify original launch bindings during recovery and compiled content operations.
