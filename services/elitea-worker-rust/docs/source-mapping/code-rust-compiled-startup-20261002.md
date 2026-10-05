# Rust compiled snapshot startup mapping — 2026-10-02

This packet covers Worker and Supervisor configuration, bootstrap, and factory composition.
The feature stays off when the optional setting is absent.
The packet does not change grants, receipts, dispatch identity, or runner commands.
It does not change backend launch code, protocol sources, or migrations.

## Source mapping

| Existing owner | Previous behavior | New behavior |
| --- | --- | --- |
| `src/config.rs::SandboxRuntimeConfig` and `RuntimeDeployConfig::validate` | No compiled startup selection. | Add strict optional `compiled_snapshot` settings to the Rust execution runtime only. Omitted settings keep the feature off. Reject null, missing fields, and unknown fields. |
| `src/sandbox/compiled_profile_config.rs` (new) | Main alone can load finite pinned release profiles. | Read the exact Main revision 1 manifest through the existing regular-file helper. Verify its pin, byte bound, canonical fields, unique image/policy/bundle selection, and native platform. |
| `src/bootstrap.rs::ProductionTransportBundle::connect` | Production `CodeRuntimeFactory` has no compiled profile. | Load the profile before trust and transport work. Check the journal column when enabled. Call the existing `CodeRuntimeFactory::with_compiled_snapshots`. |
| `src/sandbox/process.rs::Config::validate`, `run`, `run_profiles`, `run_config` | Production Supervisor has no compiled profile. | Require Rust execution and the existing content policy. Validate all local profile files before profile tasks start. Use the existing Docker and Kubernetes Supervisor constructor. Call `with_compiled_snapshots`. |
| `src/sandbox/process.rs::connect_receipts` | Probe the ordinary receipt schema. | Probe compiled intent, export, and cleanup columns only when compiled snapshots are configured. These probes only read the schema. |
| Main `internal/runtimecomposition/compiled_snapshots_config.go` (unchanged reference) | Own release manifests, bounded quotas, separate AgentState, and enabled control-frame capacity. | Keep Main authority. Each compiled grant independently matches a trusted Main release profile. |

Configuration returns the existing `Arc<SnapshotProfile>`.
It does not return an attestation from the caller.
The startup loader does not create a new transport or background loop.
The existing source-to-binding logic preserves the exact prepared request fingerprint, source, input, and timeout bindings.

## Operator contract

Put the optional object in the Worker's Rust `sandbox_runtimes[]` entry.
Put the same object in the Supervisor's Rust execution profile.

```json
{"compiled_snapshot":{"profiles_file":"/run/elitea-runtime/rust-compiled-profiles.json","profiles_sha256":"<exact lowercase64 SHA-256 of file bytes>","dependency_bundle_sha256":"<exact retained native Cargo root, or empty for built-in vendor>"}}
```

These placeholders are documentation.
They are not valid release settings.
The file path must be absolute and canonical, with no symlink.
Use an operator-controlled readonly mount.
Use a non-executable file with mode `0444` or `0644`.
Keep all parent directories under operator control.

The existing Worker and Supervisor public-file reader rejects group and other write bits.
Main also requires owner-readable files with no executable bits.

The exact shared file contract is:

```text
{"revision":1,"profiles":[{"binding":<exact Binding fields in owner order>,"dependency_bundle_sha256":<root string>},...]}
```

The file contains 1..64 records and at most 1 MiB.
The pin covers the exact file bytes.
Added whitespace, a trailing newline, reordered fields, unknown fields, and duplicate records fail validation.

The loader selects one unique record.
Its compilation image, execution image, policy, and bundle root must match local runtime configuration.
Native bundle selection also requires the configured native platform.
A second matching record fails validation.
The factory selects one trusted profile per Rust runtime.
A new Cargo closure requires an explicit trusted release profile and startup selection.

Main uses the existing `ELITEA_RUNTIME_RUST_COMPILED_SNAPSHOTS_ENABLED=true` setting.
It also uses `PROFILES_FILE`, `PROFILES_SHA256`, and finite quota and TTL settings under the existing compiled-snapshot configuration prefix.
It requires the explicit private `ELITEA_RUST_COMPILED_AGENTSTATE_DSN_FILE` setting.
It requires the exact migrated AgentState head and configured private content listener.

Worker and Supervisor must receive the same manifest bytes and pin.
Their canonical mounted paths can differ.
Do not enable only one component.

## Trusted release producer and eligibility

Root owns the release manifest producer and deployment pinning.
Populate every immutable Binding field from the trusted built runner image and retained dependency bundle.
Include the canonical backend image digest, Linux platform and target, and policy revision.
Include Cargo manifest, lock, config, and vendor hashes.
Include toolchain `-Vv`, adapter binary, wrapper, and fixed compiler flags.

Main validates the complete Binding.
The Worker and Supervisor selector must not infer these fields from a request, fixture, or mutable tag.
The tenant, project, request, and source template fields use the existing Main profile contract.
Actual grants bind the original request and scope.
Preserve canonical field order.
Publish a new file pin when the bytes change.

The cache stores one bounded executable and its strict descriptor.
It does not store companion shared libraries, arbitrary files, a target directory, or an execution filesystem.
Trusted release evidence must prove that the captured executable works with the exact runner image and bundle.
Enable a profile only after this evidence is available.
Keep dependencies requiring unprovided companion artifacts outside enabled profiles.
This startup packet does not restore files or create a general target cache.

Synthetic built-in fixture digests are not release attestations.

## Verification boundary and later gates

Private tests cover exact manifest selection and immutable binding preservation.
They cover pin corruption, canonical JSON, unknown fields, and trailing JSON rejection.
They cover version, count, and byte bounds, duplicates, and ambiguous selection.
They cover image, policy, bundle, and platform mismatches.
They cover strict optional configuration, secure regular-file loading, default-off behavior, and both backend configuration shapes.
They do not prove deployed cold/warm behavior, performance, Linux PID1 authority, or mTLS acceptance.

The independent backend packet must supply and verify trusted PID1 image and policy launch values.
Indexed native cold preparation and warm Execute hydration retain separate typed roles.
Root must assemble the required postimages and regenerate sources through owner commands.
Prove enabled Worker and Supervisor schema probes against isolated, migrated PostgreSQL.
Include the missing-migration failure case.

Run original-runtime receipt and cleanup acceptance.
Run mTLS, Linux, restart, and cold/warm acceptance.
This packet does not deploy services or change feature flags.
It does not claim a speedup.
