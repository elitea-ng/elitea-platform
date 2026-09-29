# Docker sandbox deployment

Apply `docker-compose.sandbox.yml` after the full standalone and Rust-agent overlays.
This adds separate Deno and Rust supervisor instances. Each instance admits one fixed runtime image and policy.
The worker never mounts the Docker socket. Sandbox containers receive neither the socket nor supervisor trust material.

Set these deployment inputs:

| Variable | Required value |
| --- | --- |
| `ELITEA_SANDBOX_SUPERVISOR_IMAGE` | Built supervisor image, preferably pinned by digest |
| `ELITEA_SANDBOX_DOCKER_SOCKET` | Host Docker socket path |
| `ELITEA_SANDBOX_DOCKER_GID` | Socket group accessible to supervisor UID 10001 |
| `ELITEA_SANDBOX_DENO_MATERIAL` | Prepared Deno supervisor material directory |
| `ELITEA_SANDBOX_RUST_MATERIAL` | Prepared Rust supervisor material directory |
| `ELITEA_SANDBOX_WORKER_CONFIG` | Existing worker configuration with four sandbox runtime profiles |

Run `bash deploy/scripts/gen-sandbox-certs.sh <runtime-ca-directory> <private-output-directory>` to issue server certificates.
The script preserves existing files and verifies their CA, hostname, expiry, and key pairing on rerun.
It does not install certificates or change PostgreSQL settings. Install material with the deployment UID before starting services.

Use `sandbox-supervisor.example.json` as the configuration schema example.
Store each supervisor configuration as `config.json` in its material directory.
Set a distinct stable owner for each instance. Preserve that owner and Docker daemon across process replacement.
Do not share an owner between concurrent replicas. Pending stops are partitioned by this owner.
The listener checks at most 32 unleased or expired stop records every two seconds and resumes termination.
A disconnected worker is not a stop request; only an authenticated, persisted stop enables this cleanup.
Set audiences to `dns:elitea-sandbox-deno` and `dns:elitea-sandbox-rust`, respectively.
Use server certificates with the corresponding Compose service DNS name.
The worker CA must verify these certificates; the supervisor CA must verify the worker certificate.
Copy the current Main signing verification keyring. Never give the supervisor Main's private signing key.

Provision a TLS database connection for the agentstate receipt ledger.
Apply agentstate migrations `0004` through `0006` before starting the cancellation-capable supervisor.
Migration `0005` persists cancellation intent; `0006` assigns its stable reconciliation owner.
The new supervisor requires both fields.
Apply `0007` before starting the worker with durable sandbox dispatch journaling.
The worker writes activation/digest/audience metadata before requesting a dispatch grant.
Do not apply these migrations to the product database.
Private key and database URL files require owner-only permissions and must be readable by UID 10001.
Use regular files, not symlinks. On hosts with UID remapping, install material into a dedicated volume with the correct ownership.
Do not weaken file permissions to work around UID mapping.
When a private volume replaces `/run/elitea`, create mount points for existing nested file mounts before startup.
For example, preserve the mount point for `/run/elitea/toolkit-security.json` before mounting the volume read-only.

Set the Deno profile languages to `python`, `javascript`, and `typescript`.
Set the Rust profile languages to `rust` only.
Both image digests must already exist in the supervisor's Docker daemon.
The supervisor validates cached images and does not pull a runtime image for each job.
Use the same image digest, policy revision, and supported timeout in worker and supervisor profiles.

Add four `sandbox_runtimes` entries to a private copy of the current worker runtime configuration.
Each entry contains `language`, `target`, `audience`, `image_digest`, `policy_revision`, and `timeout_seconds`.
Use `elitea-sandbox-deno:9446` for Python, JavaScript, and TypeScript.
Use `elitea-sandbox-rust:9446` for Rust. Preserve the existing worker identity and trust paths.
Do not replace deployment secrets or regenerate unrelated runtime material.

The overlay publishes no supervisor port to the host.
Docker access makes the supervisor a trusted deployment component, despite its nonroot UID.
Service startup is not an acceptance test. Verify Main grant issuance, Code execution, cancellation, and restart recovery through the deployed worker.
Verify the chat and pipeline testing interfaces before closing gate 5.
Kubernetes execution requires a separate backend and deployment; this Docker overlay does not provide it.

## Kubernetes acceptance requirements

Deploy the supervisor as a service that creates isolated execution Jobs on demand.
Use a scoped service account for Job management; the worker and execution Pods must not receive that authority.
Runtime images must be pinned and warmed on eligible nodes. Image warmup alone does not provide execution support.
Apply per-job CPU, memory, process/runtime restrictions, and bounded concurrency.
Reconcile the original Job identity and durable receipt after supervisor or worker replacement.
Persist cancellation intent before deleting or terminating a Job, and confirm termination before reporting cancellation complete.
Verify execution, Stop, lost acknowledgements, and restart recovery through the deployed UI before closing this gate.
