---
date: 2026-11-02
order: 0
---
## Simpler Helm values for a production install

**One value now fills the public URL everywhere.** Set `global.publicUrl` and the chart fills `DEPLOYMENT_URL`, the OIDC redirect URI, the authentication document's `public_origin`, the gateway's self origins, the worker's base URL and SPIFFE ID and the Ingress host, where you have not set them. `global.trustedProxyCidrs` fills both trusted-proxy lists. `certManager.issuers` names the two mTLS issuers once for every component, and `certManager.issuers.create: true` makes the chart create them, namespaced, over your existing runtime CA. **The chart refuses a values file that still holds a `<...>` placeholder**, and lists each path. Components export traces to the bundled collector by default, or turn export off when there is none. Every container requests at least 50m CPU, the GKE Autopilot minimum. `deploy/scripts/mint-secrets.sh` mints every generated secret as one flat JSON object for a secret manager. `values-gke.yaml` uses all of this; **read "Release-wide settings" in `docs/UPGRADING.md` before you upgrade an install made from it.** See [Configuration](/deployment/configuration#release-wide-settings) and [Deploy on GKE with Argo CD](/deployment/gke-argocd).
