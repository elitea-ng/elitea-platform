# Compiled Rust startup material

Compiled snapshots remain off by default. Enable them only for Rust execution.
Preparation profiles and other languages cannot enable this setting.
Deploy Main, Worker, Supervisor, and Runner implementations before activation.
Render checks do not prove compilation, import, execution, or original-receipt publication.

Use one canonical release manifest for every consumer. Preserve its exact bytes and SHA-256 pin.
Do not generate this manifest through YAML or Helm JSON conversion.
Use release metadata from the measured compilation and execution image.
The synthetic test fixture grants no release authority.

The manifest permits 1..64 records and at most 1 MiB.
Every record retains the existing nineteen-field Binding order.
Startup rejects whitespace changes, duplicate fields, invalid bindings, and ambiguous release selections.
Compilation and execution image digests must match.
Retained Cargo profiles require the exact bundle root and native platform.

## Existing material paths

| Consumer | Manifest path | Additional authority |
| --- | --- | --- |
| Compose Main | `/run/elitea-runtime/rust-compiled-profiles.json` | Private `/run/elitea-runtime/agent-checkpoint-connection` |
| Compose Worker | `/run/elitea-runtime/rust-compiled-profiles.json` | Existing Worker identity and grant checks |
| Compose Rust Supervisor | `/run/elitea-sandbox/rust/rust-compiled-profiles.json` | Existing execution profile and dependency content |
| Helm Main | `<main.runtime.material.mountPath>/rust-compiled-profiles.json` | Private sibling `agent-checkpoint-connection` |
| Helm Worker | `/run/elitea-runtime/rust-compiled-profiles.json` | Existing Worker identity and grant checks |
| Helm Rust Supervisor | `/run/elitea-sandbox/rust-compiled-profiles.json` | Existing execution profile and dependency content |

Use regular files with no symlink, execute bit, or group/other write bit.
Public manifests can use 0444 or 0644. Owner-only 0600 copies also qualify.
Keep parent directories controlled by the operator.
Keep running material mounts read-only.
Never put DSN contents in environment variables, command arguments, receipts, or logs.
Main reads its original compile receipt from a separate agentstate connection file.
That file uses owner-only permissions and contains at most 16 KiB.
The original-receipt pool permits four connections per Main replica.
Budget those connections separately from the existing runtime pools.

## Compose opt-in

Prepare the existing Worker and Rust Supervisor JSON files.
Add this object only to their Rust execution configuration:

```json
{"profiles_file":"<consumer-path>","profiles_sha256":"<exact-64-lowercase-hex-pin>","dependency_bundle_sha256":""}
```

The object's key is `compiled_snapshot`. All three string fields are required.
Empty `dependency_bundle_sha256` selects built-in vendor material.
Use the exact lowercase Cargo bundle root for retained dependencies.
Do not add the object to a Worker preparation entry.
The Supervisor requires `purpose: execution`, `languages: [rust]`, and existing dependency content.

Set `ELITEA_RUST_COMPILED_PROFILES_FILE` and `ELITEA_RUST_COMPILED_PROFILES_SHA256` to the release material.
Set these bounded positive integers:

| Variable suffix, after `ELITEA_RUST_COMPILED_` | Maximum |
| --- | --- |
| `GLOBAL_ENTRIES` | 100000 |
| `GLOBAL_BYTES` | 1099511627776 |
| `TENANT_ENTRIES` | Global entry quota |
| `TENANT_BYTES` | Global byte quota |
| `PUBLISHING_TTL_SECONDS` | 300 |
| `READY_TTL_SECONDS` | 86400 |

Run the public material preflight before launch:

```sh
python3 deploy/scripts/check-compiled-sandbox-material.py \
  --profiles-file "$ELITEA_RUST_COMPILED_PROFILES_FILE" \
  --profiles-sha256 "$ELITEA_RUST_COMPILED_PROFILES_SHA256" \
  --worker-config "$ELITEA_SANDBOX_WORKER_CONFIG" \
  --rust-config "$ELITEA_SANDBOX_RUST_MATERIAL/config.json"
```

The preflight reads bounded config documents in memory. It prints only a static result and release-profile count.
It does not replace independent startup validation.
Keep the existing `agent-checkpoint-connection` in the runtime-material source.
The installer copies this file to Main only when explicitly enabled.
It preserves Main UID 65532 and Worker UID 10001.
It copies identical manifest bytes to both existing consumer volumes.

Add `deploy/docker-compose.sandbox-compiled-rust.yml` after the existing Rust agent and sandbox overlays.
Keep preparation overlays and resolver networks unchanged.
The opt-in overlay adds no execution network access.
Compose configuration export escapes literal dollars. Preserve the export's escaping when checking a second render.
The component test checks this roundtrip.

## Helm opt-in

Set `main.runtime.rustCompiledSnapshots.enabled: true` and its exact `profilesSha256`.
The six quota fields have bounded defaults. Override them according to the deployment's storage budget.
Add `rust-compiled-profiles.json` to Main, Worker, and Supervisor material Secrets.
Each Secret must carry the identical canonical bytes.
Main's material Secret also needs `agent-checkpoint-connection`.
Existing init containers produce regular copies in bounded memory volumes.
No running container receives a new raw Secret mount.

Add `compiled_snapshot` to the Rust entry in `worker.runtime.sandboxRuntimes`.
Use the fixed Worker manifest path from the table.
Its image, policy, pin, and bundle root must select one release record.
Preserve all existing Worker fields and preparation settings.

The external Supervisor JSON must contain the execution-only object independently.
Declare matching public metadata on its chart profile:

```yaml
sandboxKubernetes:
  supervisor:
    profiles:
      - file: rust.json
        port: 9447
        purpose: execution
        languages: [rust]
        dependencyContentEnabled: true
        image_digest: sha256:<exact-image-hex>
        policy_revision: <existing-Rust-policy>
        compiled_snapshot:
          profiles_file: /run/elitea-sandbox/rust-compiled-profiles.json
          profiles_sha256: <same-Main-pin>
          dependency_bundle_sha256: ""
```

Keep the other profile declarations in that list. Helm replaces lists rather than merging their entries.
These declarations do not generate or replace private Supervisor JSON.
Helm validates public declarations; startup validates the actual external material.
For retained Cargo, declare matching `native_platform` on the Supervisor and existing Worker preparation profile.
Use only Linux GNU `arm64` or `amd64` platform values.

Helm rejects raw compiled environment overrides, invalid quotas, pin differences, and mismatched Worker/Supervisor release selections.
It rejects missing internal consumer settings when enabled.
The connection guard adds four original-receipt connections per Main replica.
With PgBouncer enabled, its server pool and these independent receipt connections must both fit the server budget.
The private receipt DSN does not inherit the product PgBouncer route.
For example, eight Main replicas add 32 direct connections to the default 52-connection pool.
That total of 84 exceeds a 100-connection server budget with 25 reserved.
For example, four standalone Main replicas require 184 connections before the reserved allowance.
A server budget of 200 with 25 reserved therefore requires a lower replica ceiling or a larger measured budget.

## Source mapping and checks

| Existing hook | Deployment change |
| --- | --- |
| Main canonical manifest and material loader | Derive file, pin, DSN, and quota environment settings |
| Compose runtime-material copy list | Copy only opt-in manifest and Main receipt DSN |
| Worker runtime JSON | Preserve exact three-field Rust `compiled_snapshot` |
| Worker init-copy allowlist | Add one manifest; use read-only running material |
| Supervisor bounded material directory | Reuse its existing read-only mount and startup loader |
| Helm connection budget | Add the separate four-connection receipt pool |

Run `python3 deploy/helm/tests/render-compiled-snapshots.py` for focused checks.
Run `task helm:lint` and the existing Worker and Kubernetes render checks.
These tests use synthetic material and invoke no Docker or Kubernetes mutation.
Live cold compilation, cached import, cancellation, and browser acceptance remain release gates.
