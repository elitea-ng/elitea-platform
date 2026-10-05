# Supervisor profile bounds and native admission

Date: 2026-10-02. Status: focused Rust tests, formatting, and Helm checks pass. Runtime deployment checks remain open.

## Current-to-new source mapping

The current reference is the applied Python and native delivery source in this repository.
This change has no legacy SDK equivalent. It corrects deployment composition and supervisor admission.

| Current behavior | Source owner | Implemented behavior |
| --- | --- | --- |
| `process.rs::run_profiles` accepts at most four profiles. | `sandbox/process.rs::MAX_RUNTIME_PROFILES`, `run_profiles`, `validate_profiles` | Accept one to eight profiles. Reject repeated durable owners and listener ports. |
| The command line accepts at most eight argument tokens, equivalent to four profile pairs. | `sandbox_main.rs::profile_paths` | Use the shared eight-profile bound. Accept sixteen argument tokens. Reject incomplete pairs and unsupported flags. |
| Native execution excludes Python. Preparation accepts one language. | `sandbox/process.rs::Config::validate` | Retain both rules. Retain the four-language count limit. |
| `material.rs::prepare` rejects the thirty-third file. | `sandbox/material.rs::MAX_MATERIAL_FILES`, `prepare` | Accept at most 64 flat regular files from one immutable Secret revision. |
| A configured native platform rejects ordinary requests without native dependencies. | `sandbox/docker_supervisor.rs::native_platform_permits`, `validate_execution` | Admit ordinary requests through the existing checks. Require every supplied native platform to match the configured platform exactly. |
| Helm accepts four profiles and four staging clients. | `deploy/helm/elitea/templates/sandbox/supervisor.yaml` | Accept at most eight profiles and eight staging clients. Retain unique files, ports, Services, and staging names. |
| Existing render cases cover two execution profiles and Python preparation. | `deploy/helm/tests/render-sandbox-kubernetes.sh` | Add seven-profile and eight-profile renders. Reject empty profiles, ninth entries, and duplicate identities at the upper bound. |

Worker source paths are relative to `services/elitea-worker-rust/src`.
Deployment source paths are relative to the repository root.

## Command-line deployment correction

The first seven-profile Docker rollout exposes a separate command-line limit.
The supervisor prints usage and restarts before it reads any profile.
The worker then reports unavailable production dependencies because the supervisor listeners are absent.
Main remains healthy. No fixture execution starts during this failure.
The corrected parser uses the same profile bound as `process.rs`.
Two focused binary tests pass: seven and eight profiles work; nine profiles and malformed arguments fail.
The corrected supervisor image starts all seven authenticated listeners in Docker.
Its immutable image identity is `sha256:482218ffd9e69b2c2a3dce14ef7c99f26a5b5715b07f9ea271522873ec08f3d7`.
The worker reconnects and starts its two delivery consumers.
Main stays healthy. Configuration, material mounts, security settings, and network aliases pass replacement read-back.
The first browser request reaches execution and completes Python preparation and execution.
It then exposes the separate native JavaScript marker correction documented in `code-javascript-native-delivery-20261002.md`.
Full native browser acceptance remains open.

## Composition and resource consequences

All-language acquisition requires seven independent profiles:

1. Retained mixed Python, JavaScript, and TypeScript execution.
2. Native JavaScript and TypeScript execution.
3. Native-capable Rust execution, including ordinary Rust requests.
4. Python preparation.
5. JavaScript preparation.
6. TypeScript preparation.
7. Rust preparation.

Native execution cannot include Python. Each preparation profile admits one language.
Each profile retains its owner, listener, audience, image, policy, and content client.
The optional eighth profile remains subject to the same bounds.

The reviewed flat material layout uses 44 files for seven profiles and 49 files for eight profiles.
This calculation reuses the existing CA, keyring, and database files.
The material copier retains its 1 MiB per-file limit and 8 MiB total limit.
It validates the complete revision before replacing destination files.
The passed 65-file test confirms that rejection preserves existing destination files.

At two database connections per profile, seven profiles permit 14 connections and eight permit 16 connections.
The existing per-profile database, concurrency, content capacity, and staging limits remain unchanged.
Eight staging clients can request up to 8 GiB in total at the existing 1 GiB per-client ceiling.
Operators must budget supervisor memory, database connections, and staging storage for their selected profiles.
The default configuration still uses two profiles and no content staging clients.
No measured capacity or throughput claim follows from these bounds.

## Compatibility and authority

Ordinary requests still require valid authorization, timeout, language, image, and policy bindings.
Native requests require a configured matching platform and the existing content authority and bundle bindings.
An unconfigured profile rejects supplied native dependencies.
The predicate change does not change request bytes, grants, receipt identity, publication, hydration, or phase clocks.
Malformed configuration still fails before listeners start.

## Implementation history and verification

1. Confirm the seven-profile plan and material count with the phase review.
2. Raise only profile, staging-client, and flat material-file counts.
3. Correct the supplied-platform predicate and preserve the other admission checks.
4. Add focused boundary, composition, and render cases.
5. Apply the equivalent nested language pattern and document native constructor errors.

The targeted Rust formatting check passes.
`bash deploy/helm/tests/render-sandbox-kubernetes.sh` passes.
`helm lint deploy/helm/elitea` passes with the existing optional gateway configuration notices.
Locked offline all-feature focused Rust tests pass with two jobs: nine process, three material, and three supervisor tests.
All 15 tests run without ignored cases.
The parent reports a passing all-target all-feature strict Clippy run after these Rust source changes.
The macOS test linker reports an unwind-table size warning. No source warning or test failure occurs.

These checks do not deploy a supervisor or exercise live TLS material.
They do not prove Docker or Kubernetes native execution, registry routes, cancellation, recovery, or runtime capacity.
Those acceptance paths remain owned by the parent task.
