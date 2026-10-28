import type { NavTab } from '../nav-types';

/** The Deployment tab (production Kubernetes deployment) — see `../nav.ts`
 * for how this is assembled and enforced. */
export const deploymentTab: NavTab = {
  kind: 'tab',
  title: 'Deployment',
  groups: [
    {
      kind: 'group',
      title: 'Plan',
      pages: [
        { kind: 'page', slug: 'deployment/overview', title: 'Production deployment overview' },
      ],
    },
    {
      kind: 'group',
      title: 'Install',
      pages: [
        { kind: 'page', slug: 'deployment/from-scratch', title: 'Deploy from scratch' },
        { kind: 'page', slug: 'deployment/gcp', title: 'Deploy on Google Cloud (GKE)' },
        { kind: 'page', slug: 'deployment/gke-argocd', title: 'Deploy on GKE with Argo CD' },
        { kind: 'page', slug: 'deployment/helm-install', title: 'Install with Helm' },
        { kind: 'page', slug: 'deployment/argocd', title: 'GitOps with ArgoCD' },
        { kind: 'page', slug: 'deployment/cloud-executor-kubernetes', title: 'Cloud executor on Kubernetes' },
      ],
    },
    {
      kind: 'group',
      title: 'Operate',
      pages: [
        { kind: 'page', slug: 'deployment/configuration', title: 'Post-install configuration' },
        { kind: 'page', slug: 'deployment/operations', title: 'Day-2 operations' },
        { kind: 'page', slug: 'deployment/scaling', title: 'Scaling' },
        { kind: 'page', slug: 'deployment/credential-governance', title: 'Enterprise credential governance' },
        { kind: 'page', slug: 'deployment/credential-governance-runbook', title: 'Credential governance runbook' },
        { kind: 'page', slug: 'deployment/hardening-checklist', title: 'Hardening checklist' },
      ],
    },
  ],
};
