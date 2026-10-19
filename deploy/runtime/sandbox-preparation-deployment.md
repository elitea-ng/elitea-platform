# Dependency preparation

## Docker deployment

The standalone compose stack prepares dependencies for every Code language by default (PLAN §8.3); there is no
overlay. `elitea-sandbox` serves four preparation profiles beside the three execution profiles:

| Profile | Port | Audience | Languages | Policy | Preparer image |
| --- | --- | --- | --- | --- | --- |
| `prep` | 9448 | `dns:elitea-sandbox-preparation` | Python (PyPI) | `python-preparation-v1` | Deno runner |
| `jsprep` | 9449 | `dns:elitea-sandbox-javascript-preparation` | JavaScript (npm) | `deno-preparation-v1` | Deno runner |
| `tsprep` | 9451 | `dns:elitea-sandbox-typescript-preparation` | TypeScript (npm) | `deno-preparation-v1` | Deno runner |
| `rsprep` | 9452 | `dns:elitea-sandbox-rust-preparation` | Rust (crates.io) | `cargo-preparation-v1` | Rust runner |

`deploy/scripts/code-material.py` (run by `standalone-stack.sh certs` and `up`) issues the leaves below with
`gen-sandbox-certs.sh --native` and renders every profile's `config.json` and the worker's `preparation` blocks with the
same image digest, policy revision and timeout on both sides. Preparers run on the stack's own `code-preparation`
network; the worker, Main and the execution containers never join it. Docker cannot limit that network's egress to
host names: the preparer's own `--allow-net` list is the destination limit (PyPI: `cdn.jsdelivr.net`, `pypi.org`,
`files.pythonhosted.org`; npm and crates.io through their registries). Every content client has its own bounded,
`noexec` 256 MiB staging tmpfs. A Code node that declares packages on a runtime without a preparation profile fails
with the typed refusal `pipeline.code_preparation_unavailable` before any sandbox work.

The sections below document the material contract the script implements, for operators who assemble it by hand.

## Issue separate TLS leaves

Use the existing runtime CA. Keep its existing certificates and private keys.

```bash
bash deploy/scripts/gen-sandbox-certs.sh "$RUNTIME_CA_DIRECTORY" "$PRIVATE_OUTPUT_DIRECTORY" --preparation
```

The optional flag adds these leaves:

| Output prefix | Exact DNS identity | Extended key usage |
| --- | --- | --- |
| `elitea-sandbox-preparation` | `elitea-sandbox-preparation` | `serverAuth` |
| `elitea-sandbox-preparation-content-client` | `elitea-sandbox-preparation` | `clientAuth` |
| `elitea-sandbox-deno-content-client` | `elitea-sandbox-deno` | `clientAuth` |

The server and content client use different certificates and keys.
Each optional leaf contains exactly one DNS SAN and one extended key usage.
Reruns verify the existing chain, purpose, exact DNS identity, expiry, and key pairing.
Reruns preserve every existing certificate and key.
The script fails on invalid existing material instead of rotating it.
Without the optional flag, the existing Deno, Rust, and PostgreSQL issuance behavior remains the same.

Use `--native` to issue the Python leaves and the additional native delivery leaves.
The script uses the same existing runtime CA.
This flag does not enable profiles, create networks, or change existing runtime identities.

```bash
bash deploy/scripts/gen-sandbox-certs.sh "$RUNTIME_CA_DIRECTORY" "$PRIVATE_OUTPUT_DIRECTORY" --native
```

| Additional output prefix | Exact DNS identity | Extended key usage |
| --- | --- | --- |
| `elitea-sandbox-deno-native` | `elitea-sandbox-deno-native` | `serverAuth` |
| `elitea-sandbox-javascript-preparation` | `elitea-sandbox-javascript-preparation` | `serverAuth` |
| `elitea-sandbox-typescript-preparation` | `elitea-sandbox-typescript-preparation` | `serverAuth` |
| `elitea-sandbox-rust-preparation` | `elitea-sandbox-rust-preparation` | `serverAuth` |
| `elitea-sandbox-rust-content-client` | `elitea-sandbox-rust` | `clientAuth` |
| `elitea-sandbox-deno-native-content-client` | `elitea-sandbox-deno-native` | `clientAuth` |
| `elitea-sandbox-javascript-preparation-content-client` | `elitea-sandbox-javascript-preparation` | `clientAuth` |
| `elitea-sandbox-typescript-preparation-content-client` | `elitea-sandbox-typescript-preparation` | `clientAuth` |
| `elitea-sandbox-rust-preparation-content-client` | `elitea-sandbox-rust-preparation` | `clientAuth` |

Use these same separate leaves for Docker and Kubernetes profiles.
Keep each profile audience equal to its sole certificate DNS identity.
Mount only the required leaf pairs and public trust into supervisor material.
Keep the CA private key outside that material.
Verify profile resources and registry policy before enabling native delivery.

Install preparation material with these names:

| Material name | Source |
| --- | --- |
| `config.json` | Private copy of `sandbox-preparation.example.json` |
| `client-ca.pem` | Existing CA that verifies the worker certificate |
| `server.pem` | `elitea-sandbox-preparation.crt` |
| `server.key` | `elitea-sandbox-preparation.key` |
| `content-ca.pem` | Existing runtime CA that verifies Main's content listener |
| `content-client.pem` | `elitea-sandbox-preparation-content-client.crt` |
| `content-client.key` | `elitea-sandbox-preparation-content-client.key` |
| `command-signing-keyring.json` | Existing Main public signing verification keyring |
| `database-url` | Existing private TLS connection material for the agentstate ledger |
| `database-ca.pem` | Existing database server trust certificate |

Install regular files readable by UID 10001.
Set private key and database URL files to owner-only permissions.
Keep CA private keys and Main private signing keys outside supervisor material.
Keep database credentials, worker credentials, and supervisor material outside preparation and execution containers.
Apply the existing agentstate receipt migrations through 0010 before starting these profiles.
Migration 0009 provides the preparation bundle receipt. Migration 0010 provides the durable phase clocks.
The material job does not apply migrations; `elitea-agentstate-migrate` runs them before the supervisor starts.

## Enable execution content downloads

Add the following object to the private Deno supervisor `config.json`:

```json
{
  "dependency_content": {
    "origin": "https://elitea-main:9445",
    "ca_path": "/run/elitea-sandbox/deno/content-ca.pem",
    "certificate_path": "/run/elitea-sandbox/deno/content-client.pem",
    "private_key_path": "/run/elitea-sandbox/deno/content-client.key",
    "staging_root": "/run/elitea-sandbox-content/deno",
    "capacity": 1,
    "timeout_seconds": 30
  }
}
```

Copy the runtime CA to Deno `content-ca.pem`.
Install the Deno content client leaf as `content-client.pem` and `content-client.key`.
Keep Deno purpose as `execution`. Omit `preparation_network` from execution profiles.
The content client verifies Main TLS and downloads the root-bound bundle before offline execution.
Its DNS identity must equal the Deno audience in the revision 3 content grant.
The preparation content client uses the preparation audience for publication.
Both content clients use Main's existing private HTTPS listener on port 9445.

The default stack mounts a separate staging filesystem for every content client.
Each tmpfs has a 256 MiB limit, UID 10001 ownership, and mode 0700.
Each tmpfs disables execution, device files, and set-user-ID behavior.
The example content clients limit concurrent transfers to one and use 30-second deadlines.
Staging is disposable. Main's published content and durable preparation receipts support replacement recovery.

## Enable the worker profile

Add `preparation` only to the Python entry in a private copy of the current worker configuration:

```json
{
  "language": "python",
  "target": "elitea-sandbox-deno:9446",
  "audience": "dns:elitea-sandbox-deno",
  "image_digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "policy_revision": "python-js-v1",
  "timeout_seconds": 60,
  "preparation": {
    "target": "elitea-sandbox-preparation:9448",
    "audience": "dns:elitea-sandbox-preparation",
    "image_digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    "policy_revision": "python-preparation-v1",
    "timeout_seconds": 120
  }
}
```

Replace both example digests with the operator-selected images.
Match the execution image and policy to the private Deno supervisor configuration.
Preserve the current worker identity, checkpoint connection, and trust paths.
On the standalone stack `code-material.py render` writes this configuration to `deploy/certs/code/worker-runtime.json`.
Keep JavaScript, TypeScript, and Rust entries under their existing profiles.
This optional deployment profile admits Python preparation only.

## Verify Docker

`python3 scripts/runtime/test_durability_defaults.py` renders the default compose and the rendered material and fails
if any preparation profile, audience, worker preparation block or the resolver network is missing. Treat a rendered
configuration as private: existing stack environment fields can contain deployment values.

## Kubernetes deployment

Use the existing consolidated supervisor Deployment. Keep its execution profiles, stable owners, cluster identity, and receipt database.
Enable the optional preparation boundary in `sandboxKubernetes.preparation`.
Use a dedicated preparation namespace. Keep it separate from the platform and offline execution namespaces.
The chart adds namespace-scoped Pod, exec, and log permissions to the existing supervisor ServiceAccount.
The preparation Pod ServiceAccount has no mounted token or API permissions.
The chart keeps the execution namespace's deny-all network policy unchanged.

Create a private `preparation.json` from `sandbox-preparation.kubernetes.example.json`.
Add it to the existing supervisor material Secret as a flat key.
Set the backend namespace to the exact Helm preparation namespace.
Match the backend image digest, worker preparation digest, policy revision, and timeout.
Keep the Kubernetes cluster identity and each profile owner stable during replacement.
Omit `preparation_network` from Kubernetes profiles. The namespace policy controls resolver egress.
Set preparation purpose to `preparation` and its languages to `["python"]`.
The process rejects preparation on other languages.

Add these optional values to the existing deployment values:

```yaml
main:
  runtime:
    # Keep all existing exact execution audiences.
    sandboxAudiences:
      - dns:elitea-sandbox-deno
      - dns:elitea-sandbox-rust
      - dns:elitea-sandbox-preparation
sandboxKubernetes:
  enabled: true
  executionNamespace: elitea-code-execution
  preparation:
    enabled: true
    namespace: elitea-python-preparation
    maxPods: 4
    dnsNamespaceLabels:
      kubernetes.io/metadata.name: kube-system
    dnsPodLabels:
      k8s-app: kube-dns
    # Replace with operator-approved public registry or controlled gateway CIDRs.
    # Documentation addresses below cannot reach the registries.
    resolverCidrs: [203.0.113.0/24]
  supervisor:
    enabled: true
    image: registry.example/elitea-sandbox-supervisor@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    materialSecret: elitea-sandbox-material
    profiles:
      - file: deno.json
        port: 9446
        serviceName: elitea-sandbox-deno
      - file: rust.json
        port: 9447
        serviceName: elitea-sandbox-rust
      - file: preparation.json
        port: 9448
        serviceName: elitea-sandbox-preparation
    contentStaging:
      - {name: deno, sizeMiB: 256}
      - {name: preparation, sizeMiB: 256}
    contentStagingInitImage: registry.example/staging-init@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
    resources:
      limits: {cpu: "1", memory: 1Gi}
```

Replace every example image and digest with the deployment's immutable image references.
Preserve the existing execution file names, ports, namespaces, audience values, resources, and image bindings.
Omit execution `serviceName` entries if existing worker targets already match their server certificates.
Each optional alias points to one listener in the consolidated supervisor. It adds no host port or external Service.
Keep the existing `elitea-sandbox-supervisor` Service. It continues to expose the configured internal listeners.

Issue the separate preparation server and content client leaves with the existing certificate helper.
Use the exact preparation server DNS identity `elitea-sandbox-preparation` for the new Service alias and worker target.
Keep the content client's sole DNS identity equal to its exact grant audience.
Install these new flat keys in the existing material Secret:

| Secret key | Source |
| --- | --- |
| `preparation.json` | Private Kubernetes preparation configuration |
| `preparation-server.pem` | `elitea-sandbox-preparation.crt` |
| `preparation-server.key` | `elitea-sandbox-preparation.key` |
| `preparation-content-client.pem` | `elitea-sandbox-preparation-content-client.crt` |
| `preparation-content-client.key` | `elitea-sandbox-preparation-content-client.key` |
| `deno-content-client.pem` | `elitea-sandbox-deno-content-client.crt` |
| `deno-content-client.key` | `elitea-sandbox-deno-content-client.key` |
| `content-ca.pem` | Existing CA for Main's content listener |

Reuse the existing worker client CA, public command keyring, database URL, and database CA keys.
Keep the Secret flat and within the material copier's 32-file and 8 MiB total limits.
The existing init container copies the selected Secret revision into regular 0600 files.
Do not mount this Secret into preparation or execution Pods.

Add `dependency_content` to the existing Deno execution configuration:

```json
{
  "dependency_content": {
    "origin": "https://elitea-main:9445",
    "ca_path": "/run/elitea-sandbox/content-ca.pem",
    "certificate_path": "/run/elitea-sandbox/deno-content-client.pem",
    "private_key_path": "/run/elitea-sandbox/deno-content-client.key",
    "staging_root": "/run/elitea-sandbox-content/deno/private",
    "capacity": 1,
    "timeout_seconds": 30
  }
}
```

Use the current Deno audience's content leaf if that audience differs from the example.
Keep Main's existing runtime plane, content listener, object storage, and workload session configuration enabled.
Add the exact preparation audience to `main.runtime.sandboxAudiences` without removing existing audiences.
Apply the receipt migrations through 0010 before starting the supervisor.

Select a trusted pinned staging init image with `/bin/sh`, `mkdir`, and `chmod`.
The init runs as UID 10001 and creates a mode 0700 `private` directory inside each memory volume.
Set every content client's staging root to that private subdirectory.
The memory volumes remain writable only in the supervisor and staging init containers.
They are disposable transfer storage. They do not hold durable receipts or replace Main's published bundle storage.
Kubernetes `emptyDir` does not expose Docker's `noexec` tmpfs option. The supervisor does not execute staging files.

Match the DNS namespace and Pod labels to the actual cluster DNS deployment.
The namespace policy permits only those DNS Pods on UDP/TCP 53 and explicit resolver CIDRs on TCP 443.
The chart rejects an empty destination list and zero-prefix address grants.
Kubernetes NetworkPolicy cannot enforce FQDN allowlists.
Select public registry CIDRs or a controlled egress gateway that serves `cdn.jsdelivr.net`, `pypi.org`, and `files.pythonhosted.org`.
Verify destination limits outside the chart. Reject platform, database, API server, node, link-local, metadata, and unrelated private routes.
Do not add broader policies to this namespace. NetworkPolicy permissions combine across policies.
DNS routing, registry address updates, and gateway rules remain operator responsibilities.

Warm every execution and preparation image on all selected sandbox nodes before admitting jobs.
Use `sandboxImageWarmup.images` with the exact registry digest references from each backend configuration.
Use the same explicit node selector as those profiles. The runtime Pods use `imagePullPolicy: Never`.
A cached Docker image ID without the configured registry digest reference is insufficient.
Select the existing sandbox RuntimeClass. Verify the existing kubelet PID limit and Pod isolation requirements.
The preparation image must contain the trusted launcher, native resolver, and `/bin/sleep`.
The supervisor and staging init images use `IfNotPresent`; supply registry access or preload those images separately.

Add the worker's `preparation` object only to its Python entry in `worker.runtime.sandboxRuntimes`.
Use target `elitea-sandbox-preparation:9448` and audience `dns:elitea-sandbox-preparation` in the same platform namespace.
Keep the worker's existing execution target, identity, checkpoint connection, and trust material.
Match the preparation image, policy, and timeout to `preparation.json`.
The chart preserves the optional object in the Rust worker configuration. Other language entries stay unchanged.

## Verify Kubernetes

Run `task helm:lint`, `deploy/helm/tests/render-sandbox-kubernetes.sh`, and `deploy/helm/tests/render-worker-sandbox.sh` during integration.
Render the deployment with the private material Secret references and operator values before startup.
Verify one supervisor Deployment, all profile arguments, internal aliases, scoped RBAC, private staging, and immutable images.
Verify the unchanged offline execution policy and the preparation resolver destination limits through the CNI.
Repeat the complete publication, hydration, execution, Stop, replacement, and original Pod UID refusal acceptance cases.
Verify persistent chat and pipeline testing through the real native backend before closing the Code acceptance gate.
This deployment source and render coverage do not establish runtime or browser acceptance.
See [deployment parity mapping](../../services/elitea-worker-rust/docs/source-mapping/code-python-deployment-parity-20261002.md).
