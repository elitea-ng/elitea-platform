import type { NavTab } from '../nav-types';

/** The Admin tab — see `../nav.ts` for how this is assembled and enforced. */
export const adminTab: NavTab = {
  kind: 'tab',
  title: 'Admin',
  groups: [
    {
      kind: 'group',
      title: 'Admin',
      pages: [
        { kind: 'page', slug: 'admin/overview', title: 'Admin console overview' },
        { kind: 'page', slug: 'admin/users-and-roles', title: 'Users and roles' },
        { kind: 'page', slug: 'admin/projects', title: 'Projects' },
        { kind: 'page', slug: 'admin/authentication', title: 'Authentication' },
        { kind: 'page', slug: 'admin/llm-proxy', title: 'LLM proxy' },
        { kind: 'page', slug: 'admin/budgets-and-governance', title: 'Budgets and LLM governance' },
        { kind: 'page', slug: 'admin/features-and-configuration', title: 'Features and configuration' },
        { kind: 'page', slug: 'admin/branding', title: 'Branding' },
        { kind: 'page', slug: 'admin/secrets', title: 'Secrets' },
        { kind: 'page', slug: 'admin/schedules-and-tasks', title: 'Schedules and tasks' },
        { kind: 'page', slug: 'admin/app-requests-and-toolkit-types', title: 'App requests and toolkit types' },
        { kind: 'page', slug: 'admin/audit-trail', title: 'Audit trail' },
      ],
    },
    {
      kind: 'group',
      title: 'Enterprise controls',
      pages: [
        { kind: 'page', slug: 'admin/enterprise-controls/overview', title: 'Control catalogue' },
        { kind: 'page', slug: 'admin/enterprise-controls/shared-responsibility', title: 'Shared responsibility' },
        { kind: 'page', slug: 'admin/enterprise-controls/runtime-safety', title: 'Runtime safety' },
        { kind: 'page', slug: 'admin/enterprise-controls/audit-evidence-and-retention', title: 'Audit evidence and retention' },
        { kind: 'page', slug: 'admin/enterprise-controls/least-privilege-and-trust', title: 'Least privilege and trust' },
        { kind: 'page', slug: 'admin/enterprise-controls/evidence-guide', title: 'Evidence guide for auditors' },
      ],
    },
  ],
};
