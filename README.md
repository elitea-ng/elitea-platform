<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/assets/logo/elitea-lockup-on-dark.svg">
    <img alt="Elitea AI" src="brand/assets/logo/elitea-lockup-on-light.svg" width="280">
  </picture>
</p>

<p align="center">
  The open platform for building, running and sharing AI agents, pipelines and toolkits.
</p>

<p align="center">
  <a href="https://elitea-ng.github.io/elitea-platform/">Documentation</a> ·
  <a href="https://elitea-ng.github.io/elitea-platform/getting-started/install">Install</a> ·
  <a href="https://github.com/elitea-ng/elitea-platform/releases">Releases</a> ·
  <a href="https://elitea-ng.github.io/elitea-platform/brand/">Brand guide</a> ·
  <a href="CONTRIBUTING.md">Contributing</a>
</p>

---

Elitea AI brings chat, agents, pipelines, toolkits and platform administration
into one workspace your organisation runs itself. You keep the models, the
credentials and the data; Elitea gives every team the same governed way to put
them to work.

- **Chat** with models, agents, pipelines, toolkits and MCP servers, and build them inline.
- **Agents and pipelines**: instructions, a model and tools, or a visual workflow that chains them.
- **Toolkits and MCP servers** connect Jira, GitHub, Confluence and many more, with credentials kept in a vault.
- **Indexing** turns documents and sources into vector indexes, so answers rest on your content.
- **DeepWiki and Inventory** turn a code repository into browsable documentation or a queryable component graph.
- **Governance**: one LLM gateway with budgets, request logs and guardrails; SSO (OIDC, SAML), roles and audit.
- **Your look**: a runtime brand pack sets the name, logo, colours and fonts without a rebuild.

## Clients

| Client | Where |
|---|---|
| Web | Served by every deployment at `/app/`; the admin console at `/admin/app/` |
| Desktop (macOS) | A Tauri app with a local agent loop. Builds are attached to [releases](https://github.com/elitea-ng/elitea-platform/releases) |
| Mobile (iOS, Android) | An Expo app that connects to a deployment by QR code (`apps/elitea-mobile`) |

## Get started

| I want to… | Start here |
|---|---|
| Try Elitea on one machine | [Install Elitea](https://elitea-ng.github.io/elitea-platform/getting-started/install) |
| Run it in production | [Production deployment overview](https://elitea-ng.github.io/elitea-platform/deployment/overview), then [Install with Helm](https://elitea-ng.github.io/elitea-platform/deployment/helm-install) |
| Work on the code | [Set up a development environment](https://elitea-ng.github.io/elitea-platform/development/overview) |
| Use or build on the brand | [Brand guide](https://elitea-ng.github.io/elitea-platform/brand/) ([`llms.txt`](https://elitea-ng.github.io/elitea-platform/brand/llms.txt) for agents) |

The whole platform on one machine, from a clone, with [podman](https://podman.io/):

```bash
deploy/scripts/standalone-stack.sh certs   # once: local mTLS material
deploy/scripts/standalone-stack.sh build   # images from source
deploy/scripts/standalone-stack.sh up      # migrations, roles and the worker's identity included
open http://localhost:8084/app/
```

[Run the standalone stack](https://elitea-ng.github.io/elitea-platform/development/standalone-stack)
covers seeding a real model, the overlays and the `check` command.

## What's in this repository

| Path | What's there |
|---|---|
| `services/` | Go: `elitea-main` (API and browser edge), `elitea-scheduler`, `elitea-llm-gateway`, `elitea-subapp-host`, `elitea-egress-proxy`. Rust: the agent worker and the DeepWiki, Inventory, vector, code-runner and document-extractor engines |
| `apps/` | `elitea-web` (web app, admin console, embedded docs), `elitea-desktop`, `elitea-mobile` |
| `libs/` | Shared Go, Rust and TypeScript libraries and the protobuf contracts |
| `deploy/` | Compose stacks, the Helm chart (`oci://ghcr.io/elitea-ng/charts/elitea`), Argo CD and kind recipes |
| `brand/` | The brand guide's sources ([`BRAND.md`](brand/BRAND.md)) |

Architecture rules for contributors and coding agents are in [`AGENTS.md`](AGENTS.md).
Images and the chart are published to `ghcr.io/elitea-ng`, signed with cosign, and
carry signed SLSA build provenance and SBOMs
([verify a release](https://elitea-ng.github.io/elitea-platform/deployment/verify-artifacts)).
The charts are listed on [Artifact Hub](https://artifacthub.io/packages/search?ts_query_web=elitea).

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md).
Report security issues privately, as described in [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE)
