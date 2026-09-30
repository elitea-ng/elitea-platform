# Docker sandbox deployment

Apply `docker-compose.sandbox.yml` after the full standalone and Rust-agent overlays.
This adds one supervisor process with Deno and Rust runtime profiles. Each profile admits one fixed runtime image and policy.
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
Set a distinct stable owner for each runtime profile. Preserve that owner and Docker daemon across process replacement.
Do not share an owner between concurrent replicas. Pending stops are partitioned by this owner.
The listener checks at most 32 unleased or expired stop records every two seconds and resumes termination.
A disconnected worker is not a stop request; only an authenticated, persisted stop enables this cleanup.
Set audiences to `dns:elitea-sandbox-deno` and `dns:elitea-sandbox-rust`, respectively.
Use server certificates with the corresponding Compose network alias.
Set Deno to port 9446 and Rust to port 9447. Both listeners run in the same supervisor container.
Use `/run/elitea-sandbox/deno/` paths in the Deno configuration.
Use `/run/elitea-sandbox/rust/` paths in the Rust configuration.
The process accepts up to four `--config` arguments. It rejects duplicate owners or listener ports before startup.
The worker CA must verify these certificates; the supervisor CA must verify the worker certificate.
Copy the current Main signing verification keyring. Never give the supervisor Main's private signing key.

Provision a TLS database connection for the agentstate receipt ledger.
Apply agentstate migrations `0004` through `0006` before starting the cancellation-capable supervisor.
Migration `0005` persists cancellation intent; `0006` assigns its stable reconciliation owner.
The new supervisor requires both fields.
Apply agentstate migration `0008` before deploying runtime identity binding.
It adds one nullable receipt column. Existing dispatched Docker receipts remain readable.
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
Use `elitea-sandbox-rust:9447` for Rust. Preserve the existing worker identity and trust paths.
Do not replace deployment secrets or regenerate unrelated runtime material.

The overlay publishes no supervisor port to the host.
Docker access makes the supervisor a trusted deployment component, despite its nonroot UID.
Service startup is not an acceptance test. Verify Main grant issuance, Code execution, cancellation, and restart recovery through the deployed worker.
Verify the chat and pipeline testing interfaces before closing gate 5.
Kubernetes execution requires a separate backend and deployment; this Docker overlay does not provide it.

## Kubernetes acceptance requirements

Deploy the supervisor as a service that creates isolated execution Pods on demand.
Use a scoped service account for Pod management; the worker and execution Pods must not receive that authority.
Runtime images must be pinned and warmed on eligible nodes. Image warmup alone does not provide execution support.
Apply per-job CPU, memory, process/runtime restrictions, and bounded concurrency.
Reconcile the original Pod identity and durable receipt after supervisor or worker replacement.
Persist cancellation intent before deleting or terminating a Pod, and confirm termination before reporting cancellation complete.
Verify execution, Stop, lost acknowledgements, and restart recovery through the deployed UI before closing this gate.

Use one Pod per durable job with `restartPolicy: Never`. Do not add a controller that automatically replaces failed Pods.
Keep the receipt finalizer until the terminal receipt is durable.
Confirm the original Pod UID and terminated container status before completing cancellation.
A missing Pod or deletion acknowledgement does not prove that an unreachable node stopped executing code.

### Kubernetes boundary preparation

The chart can prepare the execution namespace before Kubernetes runtime activation:

```yaml
sandboxKubernetes:
  enabled: true
  executionNamespace: elitea-code-execution
  supervisorServiceAccount: elitea-sandbox-supervisor
  maxPods: 128
```

Keep the execution namespace separate from the platform namespace.
Use a CNI that enforces NetworkPolicy. Verify blocked egress before enabling Code execution.
The policy denies all workload ingress and egress, including DNS.
Image loading uses the node runtime. It does not require workload egress.

The supervisor account receives namespace-scoped Pod, exec, and log access.
Execution Pods use `elitea-code`, without automatic service account token mounting.
The supervisor cannot read Secrets or change RBAC through this Role.
Pod creation remains a privileged capability within this namespace. Do not store application secrets or unrelated workloads there.

Helm retains the namespace, network policy, and Pod quota after uninstall.
Drain supervisor receipts and confirm workload termination before removing these retained resources.
This configuration only prepares the boundary. Runtime wiring and live Kubernetes acceptance remain incomplete.

The supervisor now accepts the backend object shown in `sandbox-supervisor.kubernetes.example.json`.
Omit `backend` for the existing Docker behavior.
Kubernetes profiles use in-cluster service account credentials; they do not load an operator's local kubeconfig.
Keep the cluster identifier stable across restarts. Its value becomes part of each durable Pod binding.
Build both execution images with the new runner lifecycle helper before activation.
Copy projected Secrets into canonical, owner-private files before starting the supervisor.
Direct Secret mounts use symlinks and do not satisfy the existing private-file checks.

Enable the optional supervisor after preparing its Secret:

```yaml
sandboxKubernetes:
  enabled: true
  executionNamespace: elitea-code-execution
  supervisor:
    enabled: true
    image: registry.example/supervisor@sha256:<actual-registry-digest>
    materialSecret: sandbox-material
    profiles:
      - file: deno.json
        port: 9446
      - file: rust.json
        port: 9447
```

Store configuration and trust files as flat keys in the existing Secret.
Use absolute configuration paths under `/run/elitea-sandbox`.
Give each profile a distinct durable owner and listener port.
Use the internal Service hostname in certificate identities and Main grant audiences.
The init container copies a single Secret revision into memory-backed private files.
Restart the supervisor Pod after changing the Secret. Existing copies do not hot-reload.
The same supervisor image supplies both init and serving modes. No shell image is required.
The default worker does not receive this Secret or Kubernetes service account permissions.

### Run the Kubernetes language fixture

Prepare the execution namespace, RBAC, network policy, node label, and `elitea-code` RuntimeClass first.
Cache both runtime images under immutable registry digest references.
Set `ELITEA_TEST_KUBE_CONTEXT`, `ELITEA_TEST_KUBE_NAMESPACE`, `ELITEA_TEST_KUBE_DENO_IMAGE`, and `ELITEA_TEST_KUBE_RUST_IMAGE` explicitly.
The context must select an isolated test cluster. The test creates and deletes execution Pods.

```sh
cargo test --manifest-path services/elitea-worker-rust/Cargo.toml \
  --features sandbox-supervisor --lib live_four_language_state_chain \
  -- --ignored --nocapture
```

This checks the adapter and four language runtimes. It does not replace supervisor durability or browser acceptance tests.
Failed workloads remain available for diagnosis. Delete them only after confirming termination and preserving required evidence.
