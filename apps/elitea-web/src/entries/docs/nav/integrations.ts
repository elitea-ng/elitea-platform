import type { NavTab } from '../nav-types';

/** The Integrations tab — see `../nav.ts` for how this is assembled and
 * enforced. */
export const integrationsTab: NavTab = {
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
};
