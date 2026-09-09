/**
 * Docs navigation tree: tabs -> groups -> pages (PREAMBLE decision 2).
 *
 * This is the SINGLE SOURCE of the docs site's structure. `docs-content.test.ts`
 * enforces a bijection between the `slug`s below and the `.mdx` files under
 * `content/`: every slug here must resolve to a file, and every file must
 * appear somewhere in this tree. `App.tsx` renders the left nav straight from
 * this tree, and `router.ts` builds its slug -> page lookup from the same
 * flattened list, so the three views (nav, router, tests) can never disagree
 * about what pages exist.
 *
 * Generated from the writer batches' `nav.<batch>.json` files per product-map.md
 * section 6 (proposed navigation) — tab order Home, Getting started,
 * Development, Guides, Integrations, Deployment, Admin; group/subgroup order
 * as listed there. Four legacy fragment pages (buckets-and-files, evaluation,
 * share-a-conversation, voice) were folded into their menu pages during
 * review and do not get their own nav entries or files — see the
 * embedded-docs W5 merge report. Development and Deployment were added in a
 * later docs wave (local development environment, production Kubernetes
 * deployment) — see nav.L.json / nav.P.json in that wave's staging batch.
 */

/** A single documentation page. `slug` is root-relative, no extension, no
 * leading slash (`''` is the home page) — matches the content rule that
 * internal links are written as root-relative slugs. */
export interface NavPage {
  readonly kind: 'page';
  readonly slug: string;
  readonly title: string;
}

/** A named group of pages and/or nested groups (e.g. Pipelines' "Nodes"
 * subgroup under the Guides tab). */
export interface NavGroup {
  readonly kind: 'group';
  readonly title: string;
  readonly pages: readonly NavPage[];
  readonly groups?: readonly NavGroup[];
}

/** A top-level tab, the widest unit of the nav. */
export interface NavTab {
  readonly kind: 'tab';
  readonly title: string;
  readonly groups: readonly NavGroup[];
}

export const nav: readonly NavTab[] = [
  {
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
  },
  {
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
  },
  {
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
  },
  {
    kind: 'tab',
    title: 'Guides',
    groups: [
      {
        kind: 'group',
        title: 'Chat and conversations',
        pages: [
          { kind: 'page', slug: 'how-tos/chat-conversations/how-to-use-chat-functionality', title: 'Use chat' },
          { kind: 'page', slug: 'how-tos/chat-conversations/attach-files', title: 'Attach files to a conversation' },
          { kind: 'page', slug: 'how-tos/chat-conversations/how-to-canvas', title: 'Use canvas' },
          { kind: 'page', slug: 'how-tos/chat-conversations/create-entities-from-chat', title: 'Edit agents, pipelines, toolkits and MCPs from chat' },
          { kind: 'page', slug: 'how-tos/chat-conversations/elitea-mcp-tools', title: 'Elitea MCP Tools (internal tool)' },
          { kind: 'page', slug: 'how-tos/chat-conversations/add-teammates-to-conversation', title: 'Add teammates to a conversation' },
          { kind: 'page', slug: 'how-tos/chat-conversations/use-public-items-from-chat', title: 'Use public items from chat' },
          { kind: 'page', slug: 'how-tos/chat-conversations/context-management', title: 'Manage conversation context' },
        ],
      },
      {
        kind: 'group',
        title: 'Guardrails',
        pages: [
          { kind: 'page', slug: 'how-tos/chat-conversations/sensitive-action-authorization-guardrail', title: 'Authorize sensitive actions' },
        ],
      },
      {
        kind: 'group',
        title: 'Indexing',
        pages: [
          { kind: 'page', slug: 'how-tos/indexing/indexing-overview', title: 'Index external data' },
          { kind: 'page', slug: 'how-tos/indexing/using-indexes-tab-interface', title: 'Use the Indexes tab' },
          { kind: 'page', slug: 'how-tos/indexing/indexing-tools', title: 'Index and search data with tools' },
          { kind: 'page', slug: 'how-tos/indexing/schedule-indexing', title: 'Schedule re-indexing' },
          { kind: 'page', slug: 'how-tos/indexing/index-sources', title: 'Index data from a source' },
        ],
      },
      {
        kind: 'group',
        title: 'Agents, pipelines and skills',
        pages: [
          { kind: 'page', slug: 'how-tos/agents-pipelines/build-agent-with-ai', title: 'Build an agent with AI' },
          { kind: 'page', slug: 'how-tos/agents-pipelines/entity-versioning', title: 'Version agents and pipelines' },
          { kind: 'page', slug: 'how-tos/agents-pipelines/import-export', title: 'Import and export entities' },
          { kind: 'page', slug: 'how-tos/agents-pipelines/forking', title: 'Fork an agent or toolkit' },
          { kind: 'page', slug: 'how-tos/agents-pipelines/publishing', title: 'Publish agents and skills' },
        ],
      },
      {
        kind: 'group',
        title: 'Pipelines',
        pages: [
          { kind: 'page', slug: 'how-tos/pipelines/overview', title: 'Pipelines overview' },
          { kind: 'page', slug: 'how-tos/pipelines/flow-editor', title: 'Use the flow editor' },
          { kind: 'page', slug: 'how-tos/pipelines/yaml', title: 'Edit a pipeline as YAML' },
          { kind: 'page', slug: 'how-tos/pipelines/pipeline-runs', title: 'View pipeline runs' },
          { kind: 'page', slug: 'how-tos/pipelines/structure', title: 'Pipeline structure: state, entry point and connections' },
        ],
        groups: [
          {
            kind: 'group',
            title: 'Nodes',
            pages: [
              { kind: 'page', slug: 'how-tos/pipelines/nodes/overview', title: 'Pipeline node types' },
              { kind: 'page', slug: 'how-tos/pipelines/nodes/interaction-nodes', title: 'Interaction nodes: human-in-the-loop and printer' },
              { kind: 'page', slug: 'how-tos/pipelines/nodes/execution-nodes', title: 'Execution nodes: LLM, agent, toolkit and MCP' },
              { kind: 'page', slug: 'how-tos/pipelines/nodes/control-flow-nodes', title: 'Control-flow nodes: decision, router and state' },
            ],
          },
        ],
      },
      {
        kind: 'group',
        title: 'Credentials and toolkits',
        pages: [
          { kind: 'page', slug: 'how-tos/credentials-toolkits/how-to-use-credentials', title: 'Use credentials in a toolkit' },
          { kind: 'page', slug: 'how-tos/credentials-toolkits/user-specific-credentials', title: 'Project vs. private credentials' },
          { kind: 'page', slug: 'how-tos/credentials-toolkits/test-toolkit-tools', title: 'Test toolkit tools' },
          { kind: 'page', slug: 'how-tos/credentials-toolkits/jira-automation-with-elitea', title: 'Automate Jira with Elitea' },
          { kind: 'page', slug: 'how-tos/credentials-toolkits/webhooks-and-triggers', title: 'Webhooks and pipeline triggers' },
        ],
      },
      {
        kind: 'group',
        title: 'Organising',
        pages: [
          { kind: 'page', slug: 'how-tos/entity-management/organizing-entities', title: 'Organize agents, pipelines and toolkits' },
        ],
      },
    ],
  },
  {
    kind: 'tab',
    title: 'Integrations',
    groups: [
      {
        kind: 'group',
        title: 'MCP',
        pages: [
          { kind: 'page', slug: 'integrations/mcp/mcp-server', title: 'Elitea MCP server' },
          { kind: 'page', slug: 'integrations/mcp/make-tools-available-by-mcp', title: 'Make toolkit tools available by MCP' },
          { kind: 'page', slug: 'integrations/mcp/remote-mcp', title: 'Remote MCP' },
          { kind: 'page', slug: 'integrations/mcp/prebuilt-mcp-servers', title: 'Pre-built MCP servers' },
        ],
      },
      {
        kind: 'group',
        title: 'Toolkits',
        pages: [
          { kind: 'page', slug: 'integrations/toolkits/ado_boards_toolkit', title: 'ADO boards' },
          { kind: 'page', slug: 'integrations/toolkits/ado_plans_toolkit', title: 'ADO plans' },
          { kind: 'page', slug: 'integrations/toolkits/ado_repos_toolkit', title: 'ADO repos' },
          { kind: 'page', slug: 'integrations/toolkits/ado_wiki_toolkit', title: 'ADO wiki' },
          { kind: 'page', slug: 'integrations/toolkits/aha_toolkit', title: 'Aha!' },
          { kind: 'page', slug: 'integrations/toolkits/bitbucket_toolkit', title: 'Bitbucket' },
          { kind: 'page', slug: 'integrations/toolkits/confluence_toolkit', title: 'Confluence' },
          { kind: 'page', slug: 'integrations/toolkits/figma_toolkit', title: 'Figma' },
          { kind: 'page', slug: 'integrations/toolkits/github_toolkit', title: 'GitHub' },
          { kind: 'page', slug: 'integrations/toolkits/gitlab_toolkit', title: 'GitLab' },
          { kind: 'page', slug: 'integrations/toolkits/gitlab_org_toolkit', title: 'GitLab Org' },
          { kind: 'page', slug: 'integrations/toolkits/google_places_toolkit', title: 'Google Places' },
          { kind: 'page', slug: 'integrations/toolkits/imagegen_toolkit', title: 'ImageGen' },
          { kind: 'page', slug: 'integrations/toolkits/jira_toolkit', title: 'Jira' },
          { kind: 'page', slug: 'integrations/toolkits/openapi_toolkit', title: 'OpenAPI' },
          { kind: 'page', slug: 'integrations/toolkits/postman_toolkit', title: 'Postman' },
          { kind: 'page', slug: 'integrations/toolkits/powerpoint_toolkit', title: 'PowerPoint' },
          { kind: 'page', slug: 'integrations/toolkits/qtest_toolkit', title: 'QTest' },
          { kind: 'page', slug: 'integrations/toolkits/rally_toolkit', title: 'Rally' },
          { kind: 'page', slug: 'integrations/toolkits/reportportal_toolkit', title: 'Report Portal' },
          { kind: 'page', slug: 'integrations/toolkits/salesforce_toolkit', title: 'Salesforce' },
          { kind: 'page', slug: 'integrations/toolkits/servicenow_toolkit', title: 'ServiceNow' },
          { kind: 'page', slug: 'integrations/toolkits/sharepoint_toolkit', title: 'SharePoint' },
          { kind: 'page', slug: 'integrations/toolkits/slack_toolkit', title: 'Slack' },
          { kind: 'page', slug: 'integrations/toolkits/sonar_toolkit', title: 'Sonar' },
          { kind: 'page', slug: 'integrations/toolkits/sql_toolkit', title: 'SQL' },
          { kind: 'page', slug: 'integrations/toolkits/testIO_toolkit', title: 'TestIO' },
          { kind: 'page', slug: 'integrations/toolkits/testrail_toolkit', title: 'TestRail' },
          { kind: 'page', slug: 'integrations/toolkits/xray_toolkit', title: 'XRAY Cloud' },
          { kind: 'page', slug: 'integrations/toolkits/zephyr_enterprise_toolkit', title: 'Zephyr Enterprise' },
          { kind: 'page', slug: 'integrations/toolkits/zephyr_scale_toolkit', title: 'Zephyr Scale' },
          { kind: 'page', slug: 'integrations/toolkits/artifact_toolkit', title: 'Artifact' },
          { kind: 'page', slug: 'integrations/toolkits/memory_toolkit', title: 'Memory' },
          { kind: 'page', slug: 'integrations/toolkits/custom_toolkit', title: 'Custom' },
          { kind: 'page', slug: 'integrations/toolkits/other-toolkits', title: 'Other toolkit types' },
        ],
      },
      {
        kind: 'group',
        title: 'Applications',
        pages: [
          { kind: 'page', slug: 'integrations/apps/wikis', title: 'Wikis (DeepWiki)' },
          { kind: 'page', slug: 'integrations/apps/inventory', title: 'Inventory' },
        ],
      },
      {
        kind: 'group',
        title: 'API',
        pages: [
          { kind: 'page', slug: 'integrations/third-party-integrations/api-usage', title: 'API usage' },
        ],
      },
    ],
  },
  {
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
  },
  {
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
    ],
  },
];

/** Every page in the tree, in nav (document) order — the order `router.ts`
 * uses for prev/next and `docs-content.test.ts` uses for the nav<->files
 * bijection. Depth-first: a group's own pages before its nested groups'. */
export function flattenNav(tabs: readonly NavTab[] = nav): NavPage[] {
  const pages: NavPage[] = [];
  const walkGroup = (group: NavGroup): void => {
    pages.push(...group.pages);
    for (const child of group.groups ?? []) walkGroup(child);
  };
  for (const tab of tabs) {
    for (const group of tab.groups) walkGroup(group);
  }
  return pages;
}

/** `{ prev, next }` neighbours of `slug` in nav order, either possibly absent
 * at the first/last page. */
export function neighbours(
  slug: string,
  tabs: readonly NavTab[] = nav,
): { prev: NavPage | undefined; next: NavPage | undefined } {
  const pages = flattenNav(tabs);
  const index = pages.findIndex((page) => page.slug === slug);
  if (index === -1) return { prev: undefined, next: undefined };
  return { prev: pages[index - 1], next: pages[index + 1] };
}
