# Code platform startup

The operator selects Code broker profiles in the Worker and Supervisor deployment files.
Omitted settings keep the profiles disabled.
Saved YAML selects only the `platform_client` capability boolean.
Saved YAML cannot supply limits, listener targets, images, signing policy or trust files.

Use [the operator JSON example](code-platform-startup.example.json) as a settings template.
Replace each example image digest with an admitted immutable image digest.
Keep TLS identities, audiences and targets consistent with the existing deployment.
Keep Main's explicit broker policy and signed original-visit authority enabled for this capability.
The Worker settings do not enable Main policy.

The `worker` object is a complete Worker deployment example.
Each language entry can contain one optional `platform_client` object.
That object selects `target`, `audience`, `image_digest`, `policy_revision` and `timeout_seconds`.
The object also supplies `max_calls` and `max_total_bytes`.
The existing `PlatformClientPolicy` computes the exact policy digest.
Main must use the same limits.

The limits permit 1 through 4096 calls and 1 through 67108864 total bytes.
The example uses 32 calls and 1048576 bytes.
These limits produce policy digest `a299856c1b28b5aee8488da91e8dd2cb33251c89b677733b63447179144d03da`.
Set `agent_node_recovery` to `true` and configure durable checkpoint storage.
These settings preserve the original attempt owner.

Python, JavaScript and TypeScript can share one execution listener with their pure routes.
The example uses the same exact image, audience and execution policy for these routes.
Rust uses a separate broker listener with policy `cargo-broker-execute-v1`.
The existing pure Rust selection remains unchanged.
The Worker language limit remains four. The Supervisor profile limit remains eight.

The `supervisors` array contains two separate Supervisor deployment examples.
Install each object as its own existing Supervisor configuration file.
Set `code_platform_profile` to `true` only for an admitted broker image and wrapper.
Set `code_owner_requester` to Main's exact authenticated workload identity.
Use the `execution` purpose, finite resources and offline execution policy.
Do not use a resolver network for these profiles.
Both Docker and Kubernetes use these settings.
Kubernetes also requires its existing immutable image and namespace isolation settings.

The optional broker `compiled_snapshot` object uses the existing Rust release settings.
It selects `profiles_file`, `profiles_sha256` and `dependency_bundle_sha256`.
Provide an attestation for the broker's exact image, policy and native platform.
The broker route cannot use the pure route's attestation.
Omission disables the broker cache. An existing recorded snapshot cannot become a compilation miss.
Recovery refuses an absent or changed exact cache profile.

Startup admission and focused tests do not prove deployed broker execution.
Verify original-runtime binding, effect publication, cancellation and recovery in the deployed candidate.
The deployment owner controls image publication and deployment acceptance.
