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
        { kind: 'page', slug: 'deployment/helm-install', title: 'Install with Helm' },
        { kind: 'page', slug: 'deployment/argocd', title: 'GitOps with ArgoCD' },
      ],
    },
    {
      kind: 'group',
      title: 'Operate',
      pages: [
        { kind: 'page', slug: 'deployment/configuration', title: 'Post-install configuration' },
        { kind: 'page', slug: 'deployment/operations', title: 'Day-2 operations' },
      ],
    },
  ],
};
