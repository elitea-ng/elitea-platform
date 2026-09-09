import type { NavTab } from '../nav-types';

/** The Guides tab — see `../nav.ts` for how this is assembled and enforced. */
export const guidesTab: NavTab = {
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
};
