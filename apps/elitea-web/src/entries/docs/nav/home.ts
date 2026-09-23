import type { NavTab } from '../nav-types';

/** The Home tab — see `../nav.ts` for how this is assembled and enforced. */
export const homeTab: NavTab = {
  kind: 'tab',
  title: 'Home',
  groups: [
    {
      kind: 'group',
      title: 'Welcome',
      pages: [
        { kind: 'page', slug: '', title: 'Elitea Docs' },
        { kind: 'page', slug: 'home/introduction', title: 'Overview of the Elitea platform' },
        { kind: 'page', slug: 'home/onboarding-tips', title: 'Onboarding tips' },
      ],
    },
    {
      kind: 'group',
      title: 'Key concepts',
      pages: [
        { kind: 'page', slug: 'home/key-concepts', title: 'Key concepts' },
        { kind: 'page', slug: 'home/glossary', title: 'Glossary' },
      ],
    },
    {
      kind: 'group',
      title: 'Platform overview',
      pages: [
        { kind: 'page', slug: 'menus/overview', title: 'Platform overview' },
        { kind: 'page', slug: 'menus/chat', title: 'Chat' },
        { kind: 'page', slug: 'menus/agents', title: 'Agents' },
        { kind: 'page', slug: 'menus/pipelines', title: 'Pipelines' },
        { kind: 'page', slug: 'menus/skills', title: 'Skills' },
        { kind: 'page', slug: 'menus/toolkits', title: 'Toolkits' },
        { kind: 'page', slug: 'menus/applications', title: 'Applications' },
        { kind: 'page', slug: 'menus/credentials', title: 'Credentials' },
        { kind: 'page', slug: 'menus/artifacts', title: 'Artifacts' },
        { kind: 'page', slug: 'menus/catalog', title: 'Catalog' },
        { kind: 'page', slug: 'menus/deepwiki-and-inventory', title: 'DeepWiki and Inventory' },
      ],
    },
    {
      kind: 'group',
      title: 'Help',
      pages: [
        { kind: 'page', slug: 'menus/help-and-support', title: 'Help and support' },
      ],
    },
    {
      kind: 'group',
      title: 'Settings',
      pages: [
        { kind: 'page', slug: 'menus/settings/settings-overview', title: 'Settings overview' },
        { kind: 'page', slug: 'menus/settings/project-settings', title: 'Project settings' },
        { kind: 'page', slug: 'menus/settings/ai-providers', title: 'AI Providers' },
        { kind: 'page', slug: 'menus/settings/analytics-and-usage', title: 'Analytics and usage' },
        { kind: 'page', slug: 'menus/settings/personal-settings', title: 'Personal settings' },
        { kind: 'page', slug: 'menus/settings/memory', title: 'Memory' },
        { kind: 'page', slug: 'menus/settings/personal-tokens', title: 'Personal tokens' },
        { kind: 'page', slug: 'menus/settings/notifications', title: 'Notifications' },
      ],
    },
    {
      kind: 'group',
      title: 'Chat internal tools',
      pages: [
        { kind: 'page', slug: 'menus/internal-tools', title: 'Internal tools' },
      ],
    },
    {
      kind: 'group',
      title: 'Reference',
      pages: [
        { kind: 'page', slug: 'what-is-new', title: 'What\'s new' },
        { kind: 'page', slug: 'support/faqs', title: 'Frequently asked questions' },
        { kind: 'page', slug: 'support/troubleshooting', title: 'Troubleshooting' },
        { kind: 'page', slug: 'reference/toolkit-catalogue', title: 'Toolkit catalogue' },
        { kind: 'page', slug: 'reference/pipeline-node-reference', title: 'Pipeline node reference' },
      ],
    },
  ],
};
