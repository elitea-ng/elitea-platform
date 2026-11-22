---
date: 2026-11-22
---
## GitOps syncs no longer fail on the NATS bootstrap certificates

On an Argo CD-managed install, a sync could fail on a NATS bootstrap Certificate with `Issuing certificate as Secret does not exist`, even though the certificate was issued a second later, and retrying did not help. The bootstrap Job now creates these certificates itself and waits until they are ready, so the sync no longer depends on that moment. See [GitOps with ArgoCD](/deployment/argocd).
