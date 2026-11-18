import { fireEvent, screen, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getGetPlatformSettingsMockHandler } from '@/shared/api/generated/admin/admin.msw';
import { getGetApplicationMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { getListToolkitInstancesMockHandler } from '@/shared/api/generated/toolkits/toolkits.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { VersionToolRef } from '@/shared/api/generated/model';
import { server } from '@/test/setup';

import { renderWithRouterAndProject } from '../__tests__/testUtils';

import { AgentToolsPanel } from './AgentToolsPanel';

/**
 * #307's mount test. Every assertion here is about a REQUEST ON THE WIRE or
 * a control's absence — not about a card having rendered. Both tool writes
 * bypass the page's Save button and hit the relation endpoint directly, so
 * "the menu opened" / "the card is in the document" would have passed
 * against the very defects this panel exists to fix.
 */
const ATTACHED_TOOL: VersionToolRef = {
  // `id` (the entity_tool_mapping row) and `tool_id` (the toolkit instance)
  // deliberately differ — a detach addressed by `id` hits an unrelated
  // toolkit instance, project-wide. See `lib/toolRelation.ts`.
  id: 5,
  tool_id: 77,
  entity_type: 'agent',
  name: 'Github',
  type: 'github',
  config: { url: 'https://github.example' },
};

function applicationDetail(status = 'draft') {
  return {
    id: '42',
    name: 'Helper Bot',
    description: '',
    icon: '',
    owner_id: 'user-1',
    created_at: '2026-01-01T00:00:00Z',
    versions: [],
    version_details: { id: '100', application_id: '42', name: 'base', status, tools: [ATTACHED_TOOL], meta: {} },
  };
}

function platformSettings() {
  return {
    chat_enabled: true,
    applications_enabled: true,
    skills_enabled: true,
    toolkits_enabled: true,
    datasources_enabled: true,
    pipelines_enabled: true,
    publishing_enabled: true,
    moderation_enabled: true,
    mcp_enabled: false,
    support_chat_enabled: true,
  };
}

/** The real attach/detach route — no orval wrapper exists for it, so no generated msw handler does either (same reason `ToolMenu.test.tsx` hand-writes its own). */
function relationMockHandler(
  capture: (body: Record<string, unknown>, params: Readonly<Record<string, string | readonly string[] | undefined>>) => void,
) {
  return http.patch('*/elitea_core/tool/prompt_lib/:projectId/:toolkitId', async ({ request, params }) => {
    capture((await request.json()) as Record<string, unknown>, params);
    return HttpResponse.json({ message: 'ok' }, { status: 201 });
  });
}

interface PanelOverrides {
  readonly readOnly?: boolean;
  readonly versionStatus?: string;
  readonly onToolsChanged?: () => void;
}

function renderPanel({ readOnly = false, versionStatus = 'draft', onToolsChanged }: PanelOverrides = {}) {
  return renderWithRouterAndProject(
    <AgentToolsPanel
      entity={{ applicationId: 42, versionId: 100, versionStatus }}
      versionTools={[ATTACHED_TOOL]}
      dirty={false}
      internalTools={{ value: [], onChange: vi.fn() }}
      onToolsChanged={onToolsChanged}
      readOnly={readOnly}
      viewMode="owner"
    />,
    'proj-1',
  );
}

/**
 * The same attached toolkit, with a tool list to pick from: `available_tools`
 * rides in the joined toolkit blob (`config`) and `selected_tools` is a
 * TOP-LEVEL wire field — see `lib/versionTools.ts`.
 */
const SELECTABLE_TOOL: VersionToolRef = {
  ...ATTACHED_TOOL,
  selected_tools: ['create_issue'],
  config: { url: 'https://github.example', available_tools: ['create_issue', 'get_issue'] },
};

function renderPanelWithSelectableTools() {
  return renderWithRouterAndProject(
    <AgentToolsPanel
      entity={{ applicationId: 42, versionId: 100, versionStatus: 'draft' }}
      versionTools={[SELECTABLE_TOOL]}
      dirty={false}
      internalTools={{ value: [], onChange: vi.fn() }}
      readOnly={false}
      viewMode="owner"
    />,
    'proj-1',
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})),
    getGetPlatformSettingsMockHandler(platformSettings()),
    getGetApplicationMockHandler(applicationDetail()),
    getListToolkitInstancesMockHandler({
      rows: [
        {
          id: 'tk-9',
          name: 'Jira',
          type: 'jira',
          description: 'Issue tracker',
          settings: {},
          meta: {},
          created_at: '2026-01-01T00:00:00Z',
          author_id: 1,
        },
      ],
      total: 1,
    }),
    http.get('*/elitea_core/applications/prompt_lib/:projectId', () => HttpResponse.json({ rows: [], total: 0 })),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('AgentToolsPanel', () => {
  it('renders a visible card for each attached tool', async () => {
    renderPanel();
    // toBeVisible, not toBeInTheDocument: an empty accordion body satisfies
    // the latter, which is exactly how this page shipped a hollow panel once.
    await waitFor(() => expect(screen.getByTestId('agent-toolkit-card')).toBeVisible());
    expect(screen.getByText('Github')).toBeVisible();
  });

  it('DETACH: removing a card issues PATCH tool/{tool_id} with has_relation:false and drops the card', async () => {
    let body: Record<string, unknown> | undefined;
    let params: Readonly<Record<string, string | readonly string[] | undefined>> | undefined;
    server.use(relationMockHandler((b, p) => { body = b; params = p; }));
    renderPanel();

    await waitFor(() => expect(screen.getByTestId('agent-toolkit-delete-button')).toBeEnabled());
    fireEvent.click(screen.getByTestId('agent-toolkit-delete-button'));
    fireEvent.click(await screen.findByRole('button', { name: 'Remove' }));

    await waitFor(() => expect(body).toMatchObject({ entity_version_id: 100, entity_id: 42, entity_type: 'agent', has_relation: false }));
    // `toolkitId` is 77 (`tool_id`), NOT 5 (`id`) — see ATTACHED_TOOL's comment.
    expect(params).toMatchObject({ projectId: 'proj-1', toolkitId: '77' });
    await waitFor(() => expect(screen.queryByTestId('agent-toolkit-card')).not.toBeInTheDocument());
  });

  it('ATTACH: picking a toolkit from the menu issues PATCH tool/{toolkit} with has_relation:true and asks the caller to refetch', async () => {
    let body: Record<string, unknown> | undefined;
    let params: Readonly<Record<string, string | readonly string[] | undefined>> | undefined;
    const onToolsChanged = vi.fn();
    server.use(relationMockHandler((b, p) => { body = b; params = p; }));
    renderPanel({ onToolsChanged });

    await waitFor(() => expect(screen.getByTestId('agent-add-toolkit-button')).toBeEnabled());
    fireEvent.click(screen.getByTestId('agent-add-toolkit-button'));
    fireEvent.click(await screen.findByText('Jira'));

    await waitFor(() => expect(body).toMatchObject({ entity_version_id: 100, entity_id: 42, has_relation: true }));
    expect(params).toMatchObject({ projectId: 'proj-1', toolkitId: 'tk-9' });
    // Without this the attach persisted but the panel kept showing the stale
    // list until a manual reload: `ApplicationTools` rendered `ToolMenu` with
    // no `onToolsChanged` at all.
    await waitFor(() => expect(onToolsChanged).toHaveBeenCalled());
  });

  it('SELECTED TOOLS: checking a tool puts the full new list on the wire, on the relation route', async () => {
    // #248. The panel used to write this straight into its in-memory mirror
    // and stop there, so "the checkbox toggled" / "the card re-rendered" was
    // true the whole time the edit was being discarded — the assertion has to
    // be the REQUEST. The Go handler upserts the value into
    // `entity_tool_mapping.selected_tools`, keyed on the presence of this
    // exact key.
    let body: Record<string, unknown> | undefined;
    let params: Readonly<Record<string, string | readonly string[] | undefined>> | undefined;
    server.use(relationMockHandler((b, p) => { body = b; params = p; }));
    renderPanelWithSelectableTools();

    fireEvent.click(await screen.findByTestId('base-card-body-toggle'));
    fireEvent.click(await screen.findByText('Get issue'));

    await waitFor(() =>
      expect(body).toMatchObject({
        entity_version_id: 100,
        entity_id: 42,
        entity_type: 'agent',
        has_relation: true,
        selected_tools: ['create_issue', 'get_issue'],
      }),
    );
    // Same id space as attach/detach: `tool_id`, not the mapping row's `id`.
    expect(params).toMatchObject({ projectId: 'proj-1', toolkitId: '77' });
  });

  it('SELECTED TOOLS: an ATTACH leaves the key OFF the wire, so it cannot wipe a saved selection', async () => {
    // The absent-vs-empty rule, from the client side. The handler preserves a
    // stored selection only for a request with no `selected_tools` key at all
    // — a client that helpfully sent `selected_tools: []` here would erase the
    // user's selection on every re-attach.
    let body: Record<string, unknown> | undefined;
    server.use(relationMockHandler((b) => { body = b; }));
    renderPanel();

    await waitFor(() => expect(screen.getByTestId('agent-add-toolkit-button')).toBeEnabled());
    fireEvent.click(screen.getByTestId('agent-add-toolkit-button'));
    fireEvent.click(await screen.findByText('Jira'));

    await waitFor(() => expect(body).toMatchObject({ has_relation: true }));
    expect(body).not.toHaveProperty('selected_tools');
  });

  it('offers NO per-tool variables control on any card', async () => {
    // Composition-level companion to `AgentToolRow.test.tsx`'s own assertion
    // (which supplies a tool that carries variables — nothing the server sends
    // ever does, since `fetchVersionDetails` emits no `variables` key).
    renderPanelWithSelectableTools();

    expect(await screen.findByTestId('agent-toolkit-card')).toBeVisible();
    expect(screen.queryByText('Show variables')).not.toBeInTheDocument();
    expect(screen.queryByTestId('agent-variables')).not.toBeInTheDocument();
  });

  it('READ-ONLY view offers neither control: no attach menu, and remove is disabled', async () => {
    renderPanel({ readOnly: true });

    await waitFor(() => expect(screen.getByTestId('agent-toolkit-card')).toBeVisible());
    expect(screen.queryByTestId('agent-add-toolkit-button')).not.toBeInTheDocument();
    expect(screen.getByTestId('agent-toolkit-delete-button')).toBeDisabled();
  });

  it('a PUBLISHED version disables removal even for a writer (the server rejects tool changes on one)', async () => {
    renderPanel({ versionStatus: 'published' });

    await waitFor(() => expect(screen.getByTestId('agent-toolkit-card')).toBeVisible());
    expect(screen.getByTestId('agent-toolkit-delete-button')).toBeDisabled();
  });

  it('shows the delegation state of an attached agent on its row, and none on a toolkit row (ADR-0044 amendment 1)', async () => {
    const SUB_AGENT: VersionToolRef = { id: 6, tool_id: 78, entity_type: 'agent', name: 'Git specialist', type: 'application', config: { application_id: '7', application_version_id: '70' } };
    server.use(
      http.get('*/elitea_core/delegation_grants/prompt_lib/:projectId/:applicationId', () =>
        HttpResponse.json({
          grants: [
            {
              id: 5, caller_application_id: 42, callee_application_id: 7, edge_kind: 'agent_tool', callee_version_id: 70, state: 'stale', legacy: false, governed: false, origin: 'attach',
              digest: 'a', requested_digest: 'b', items: [], requested_items: [], omitted: [], created_at: '2026-11-17T10:00:00Z', updated_at: '2026-11-17T10:00:00Z',
            },
          ],
        }),
      ),
    );
    renderWithRouterAndProject(
      <AgentToolsPanel
        entity={{ applicationId: 42, versionId: 100, versionStatus: 'draft' }}
        versionTools={[ATTACHED_TOOL, SUB_AGENT]}
        dirty={false}
        internalTools={{ value: [], onChange: vi.fn() }}
        readOnly={false}
        viewMode="owner"
      />,
      'proj-1',
    );

    const status = await screen.findByTestId('delegation-grant-status');
    expect(status).toHaveTextContent('Needs re-approval');
    expect(screen.getAllByTestId('delegation-grant-status')).toHaveLength(1);
  });
});
