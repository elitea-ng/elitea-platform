import type { NavTab } from '../nav-types';

/** The Development tab (local development environment) — see `../nav.ts`
 * for how this is assembled and enforced. */
export const developmentTab: NavTab = {
  kind: 'tab',
  title: 'Development',
  groups: [
    {
      kind: 'group',
      title: 'Set up',
      pages: [
        { kind: 'page', slug: 'development/overview', title: 'Set up a development environment' },
      ],
    },
    {
      kind: 'group',
      title: 'Run the platform',
      pages: [
        { kind: 'page', slug: 'development/standalone-stack', title: 'Run the standalone stack' },
        { kind: 'page', slug: 'development/e2e-stack', title: 'Run the browser test stack' },
        { kind: 'page', slug: 'development/kind', title: 'Run a local Kubernetes cluster' },
      ],
    },
    {
      kind: 'group',
      title: 'Work on the code',
      pages: [
        { kind: 'page', slug: 'development/frontend', title: 'elitea-web development' },
        { kind: 'page', slug: 'development/backend', title: 'Backend services and workers' },
      ],
    },
  ],
};
