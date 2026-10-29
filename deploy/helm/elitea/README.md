# elitea

Elitea AI, as one Helm chart. It installs the whole first-party platform from a
single values file:

- the API (elitea-main) and the web UI (elitea-web)
- the scheduler
- the LLM gateway
- the agent worker
- the vector store: elitea-vector, with Qdrant bundled
- the DeepWiki and Inventory providers
- the runtime dispatch bus: NATS JetStream, bundled
- the telemetry collector

PostgreSQL and object storage stay outside the chart, behind one connection
contract.

## Install

```bash
helm install elitea oci://ghcr.io/elitea-ng/charts/elitea \
  --version <version> --namespace elitea --create-namespace \
  --values my-values.yaml
```

The chart has no `latest` tag; pin `--version`. The chart's own defaults do not
render on their own. The values only you know (the authentication document, the
public project, the deployment URL, the gateway's origins and egress posture)
are listed together in one refusal, so a first `helm template` is the checklist.

## What you bring

| Dependency | Notes |
|---|---|
| Kubernetes, Helm 4.3+ (Helm 3.8+ renders too) | |
| PostgreSQL | Plain PostgreSQL. No extension is needed: every vector is in Qdrant. |
| S3-compatible object storage | The bucket must exist before the first sync. |
| cert-manager | Issues the bundled NATS CA and the in-cluster mTLS certificates. |
| An ingress controller or Gateway API, DNS and TLS | One `/` route to elitea-main. |
| An OIDC or SAML identity provider | |

Secrets never go in values. Create them first, or let the chart's
`externalSecrets` block fill them from your secret manager. The chart takes only
their names.

## Verify before you install

The chart and every image are signed with cosign (keyless), and carry signed
SLSA build provenance. Each image also carries a CycloneDX SBOM.

```bash
gh attestation verify oci://ghcr.io/elitea-ng/charts/elitea:<version> \
  --repo elitea-ng/elitea-platform \
  --signer-workflow elitea-ng/elitea-platform/.github/workflows/publish.yml
```

## Documentation

- [Production deployment overview](https://elitea-ng.github.io/elitea-platform/deployment/overview)
- [Deploy from scratch](https://elitea-ng.github.io/elitea-platform/deployment/from-scratch): every Secret, certificate authority and database step, with a complete values file
- [Install with Helm](https://elitea-ng.github.io/elitea-platform/deployment/helm-install)
- [Verify release artifacts](https://elitea-ng.github.io/elitea-platform/deployment/verify-artifacts)
- [Upgrading](https://github.com/elitea-ng/elitea-platform/blob/main/docs/UPGRADING.md): read the entries between your version and the new one before an upgrade

## License

Apache-2.0.
