# Code platform startup mapping, 2026-10-05

This change connects the existing operator-owned broker contracts to production startup.
It adds no authored trust field or graph capability.
Main keeps its original-visit and broker authorization ownership.

| Current source | Changed source | Result |
| --- | --- | --- |
| `sandbox/process.rs::Config` | Add default-false `code_platform_profile` | Require execution, original owner, immutable image and finite offline resources. |
| `sandbox/process.rs::connect_runtime` | Select the admitted broker profile | Enable the existing Docker and Kubernetes mailbox hooks. |
| `config.rs::SandboxRuntimeConfig` | Add optional typed `platform_client` | Select one operator broker route per language without increasing profile caps. |
| `config.rs::RuntimeDeployConfig::validate` | Validate broker route and exact audience | Require durable original-attempt ownership and unambiguous stop delivery. |
| `bootstrap.rs::ProductionTransportBundle::connect` | Load broker attestation and connect its exact transport | Call the existing factory broker constructor before command intake. |
| `code_remote.rs::CodeRuntimeFactory::with_platform_client` | Preserve pure routes and validate broker request and attestation | Compute the existing Main-compatible policy binding. |
| `code_compiled.rs::select_compiled_snapshot_typed` | Select the route's exact cache profile | Refuse pure-profile fallback for broker Execute. |
| Startup test modules and operator JSON | Add bounded admission and compatibility fixtures | Check default refusal, both backends, pure bytes, policy digest and independent cache profiles. |

The saved source and plan fields keep their existing omission compatibility.
The pure prepared request bytes remain unchanged when a broker route is added.
The Worker language cap remains four. The Supervisor profile cap remains eight.
Python, JavaScript and TypeScript can share their existing execution listener.
Rust uses an explicit `cargo-broker-execute-v1` listener.

Broker cache omission keeps that route's cache disabled.
Recorded snapshot recovery still requires its exact profile.
No cache selection becomes a silent ordinary compilation fallback.
Original runtime, cancellation, grants and effect publication keep their existing owners.

See `deploy/runtime/code-platform-startup.md` and its exact operator JSON example.
Focused native tests use synthetic transports and do not contact infrastructure.
Root owns fresh release attestation, image builds and deployed acceptance.
This change performs no provider operation, deployment, database mutation, commit or push.
