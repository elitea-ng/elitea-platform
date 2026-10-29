import type { NavTab } from '../nav-types';

/** The Getting started tab — see `../nav.ts` for how this is assembled and
 * enforced. */
export const gettingStartedTab: NavTab = {
  kind: 'tab',
  title: 'Getting started',
  groups: [
    {
      kind: 'group',
      title: 'Install',
      pages: [
        { kind: 'page', slug: 'getting-started/install', title: 'Install Elitea' },
      ],
    },
    {
      kind: 'group',
      title: 'Quick start',
      pages: [
        { kind: 'page', slug: 'getting-started/chat-quick-start', title: 'Chat quick start' },
        { kind: 'page', slug: 'getting-started/connect-toolkits-quick-start', title: 'Connect a toolkit quick start' },
      ],
    },
    {
      kind: 'group',
      title: 'Setup',
      pages: [
        { kind: 'page', slug: 'getting-started/configure-ai-provider', title: 'Configure an AI provider' },
        { kind: 'page', slug: 'getting-started/create-personal-access-token', title: 'Create a personal access token' },
        { kind: 'page', slug: 'getting-started/create-secret', title: 'Create a secret' },
        { kind: 'page', slug: 'getting-started/create-credential', title: 'Create a credential' },
        { kind: 'page', slug: 'getting-started/create-artifact', title: 'Create an artifact bucket' },
      ],
    },
  ],
};
