import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { getGetPlatformSettingsMockHandler } from '@/shared/api/generated/admin/admin.msw';
import {
  getGetApplicationMockHandler,
  getListApplicationsMockHandler,
  getListDelegationGrantsMockHandler,
  getPreviewDelegationGrantMockHandler,
  getUpdateApplicationRelationMockHandler,
} from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { DelegationGrant, DelegationItem, DelegationPreview } from '@/shared/api/generated/model';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { getListToolkitInstancesMockHandler } from '@/shared/api/generated/toolkits/toolkits.msw';
import { server } from '@/test/setup';

import { ToolMenu } from './ToolMenu';

/**
 * The attach of an agent through the review of what it brings (ADR-0044
 * amendment 1, epic 2156 story S3): the dialog before the attach, the approval
 * after it, partial approval, the governed profile and the failure that never
 * undoes the attach.
 */
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const GIT: DelegationItem = { kind: 'toolkit', name: 'toolkit:12' };
const GIT_PUSH: DelegationItem = { kind: 'tool', name: 'push', toolkit: 'toolkit:12' };
const DB: DelegationItem = { kind: 'toolkit', name: 'toolkit:13' };

function preview(overrides: Partial<DelegationPreview> = {}): DelegationPreview {
  return {
    callee_application_id: 7,
    callee_version_id: 100,
    callee_name: 'Other Bot',
    digest: 'abc',
    items: [GIT, GIT_PUSH, DB],
    approvable: [GIT, GIT_PUSH, DB],
    withheld: [],
    closure: [],
    governed: false,
    ...overrides,
  };
}

function pendingGrant(overrides: Partial<DelegationGrant> = {}): DelegationGrant {
  return {
    id: 5,
    caller_application_id: 42,
    callee_application_id: 7,
    edge_kind: 'agent_tool',
    callee_version_id: 100,
    state: 'pending',
    legacy: false,
    governed: false,
    origin: 'attach',
    digest: '',
    requested_digest: 'abc',
    items: [],
    requested_items: [GIT, GIT_PUSH, DB],
    omitted: [],
    created_at: '2026-11-17T10:00:00Z',
    updated_at: '2026-11-17T10:00:00Z',
    ...overrides,
  };
}

function applicationDetail() {
  return {
    id: '42',
    name: 'Helper Bot',
    description: '',
    icon: '',
    owner_id: 'user-1',
    created_at: '2026-01-01T00:00:00Z',
    versions: [],
    version_details: { id: '100', application_id: '42', name: 'base', status: 'draft', tools: [], meta: {} },
  };
}

function renderMenu(onToolsChanged: () => void) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  function Root() {
    return (
      <QueryClientProvider client={queryClient}>
        <ThemeProvider
          theme={theme}
          defaultMode={DEFAULT_COLOR_SCHEME}
        >
          <ToolMenu
            applicationId={42}
            onToolsChanged={onToolsChanged}
          />
        </ThemeProvider>
      </QueryClientProvider>
    );
  }
  const rootRoute = createRootRoute({ component: Root });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => 'proj-1' } },
  });
  render(<RouterProvider router={router} />);
}

interface Calls {
  relations: number;
  approvals: { grantId: string; body: unknown }[];
}

function serve(options: { readonly preview: DelegationPreview | 'unreadable'; readonly grants?: DelegationGrant[]; readonly approve?: 'ok' | 'fail' }): Calls {
  const calls: Calls = { relations: 0, approvals: [] };
  server.use(getGetApplicationMockHandler(applicationDetail()));
  server.use(getGetPlatformSettingsMockHandler({ chat_enabled: true, applications_enabled: true, skills_enabled: true, toolkits_enabled: true, datasources_enabled: true, pipelines_enabled: true, publishing_enabled: true, moderation_enabled: true, mcp_enabled: false, support_chat_enabled: true }));
  server.use(getListToolkitInstancesMockHandler({ rows: [], total: 0 }));
  server.use(
    getListApplicationsMockHandler((info) => {
      const url = new URL(info.request.url);
      if (url.searchParams.get('agents_type') !== 'classic') return { rows: [], total: 0, page: 0, page_size: 20, total_pages: 0 };
      return {
        rows: [{ id: '7', name: 'Other Bot', owner_id: 'user-1', created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z', is_forked: false, meta: {}, has_interrupt: false, tags: [] }],
        total: 1,
        page: 0,
        page_size: 20,
        total_pages: 1,
      };
    }),
  );
  server.use(
    options.preview === 'unreadable'
      ? http.get('*/elitea_core/delegation_preview/prompt_lib/:projectId/:applicationId', () => HttpResponse.json({ error: 'boom' }, { status: 500 }))
      : getPreviewDelegationGrantMockHandler(options.preview),
  );
  server.use(
    getUpdateApplicationRelationMockHandler(() => {
      calls.relations += 1;
      return { application_id: '42', version_id: '100', has_relation: true };
    }),
  );
  server.use(getListDelegationGrantsMockHandler({ grants: options.grants ?? [pendingGrant()] }));
  server.use(
    http.post('*/elitea_core/delegation_grant/prompt_lib/:projectId/:applicationId/:grantId/approve', async ({ request, params }) => {
      const text = await request.text();
      calls.approvals.push({ grantId: String(params['grantId']), body: text === '' ? undefined : JSON.parse(text) });
      if (options.approve === 'fail') return HttpResponse.json({ error: 'nothing is left to approve' }, { status: 400 });
      return HttpResponse.json(pendingGrant({ state: 'approved' }));
    }),
  );
  return calls;
}

async function chooseOtherBot() {
  await waitFor(() => expect(screen.getByRole('button', { name: 'Agent' })).not.toBeDisabled());
  fireEvent.click(screen.getByRole('button', { name: 'Agent' }));
  fireEvent.click(await screen.findByText('Other Bot'));
}

beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
afterEach(() => resetGeneratedClient());

describe('ToolMenu attach review', () => {
  it('shows what the agent brings before anything is attached, then attaches and approves all in one step', async () => {
    const calls = serve({ preview: preview() });
    let changed = 0;
    renderMenu(() => (changed += 1));

    await chooseOtherBot();

    const dialog = await screen.findByRole('dialog', { name: 'What Other Bot will be able to use' });
    expect(within(dialog).getByRole('checkbox', { name: 'Toolkit 12' })).toBeChecked();
    expect(calls.relations).toBe(0);

    fireEvent.click(within(dialog).getByRole('button', { name: 'Approve and attach' }));

    await waitFor(() => expect(changed).toBe(1));
    expect(calls.relations).toBe(1);
    expect(calls.approvals).toEqual([{ grantId: '5', body: undefined }]);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('approves only the toolkits that stay ticked', async () => {
    const calls = serve({ preview: preview() });
    renderMenu(() => undefined);

    await chooseOtherBot();
    const dialog = await screen.findByRole('dialog');
    fireEvent.click(within(dialog).getByRole('checkbox', { name: 'Toolkit 12' }));
    fireEvent.click(within(dialog).getByRole('button', { name: 'Approve and attach' }));

    await waitFor(() => expect(calls.approvals).toHaveLength(1));
    expect(calls.approvals[0]?.body).toEqual({ items: [DB] });
  });

  it('attaches nothing when the review is cancelled', async () => {
    const calls = serve({ preview: preview() });
    renderMenu(() => undefined);

    await chooseOtherBot();
    fireEvent.click(await screen.findByRole('button', { name: 'Cancel' }));

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(calls.relations).toBe(0);
    expect(calls.approvals).toEqual([]);
  });

  it('under the governed profile says Request approval, attaches, and approves nothing (a different approver decides)', async () => {
    const calls = serve({ preview: preview({ governed: true }), grants: [pendingGrant({ governed: true, change_request_id: 9 })] });
    let changed = 0;
    renderMenu(() => (changed += 1));

    await chooseOtherBot();
    fireEvent.click(await screen.findByRole('button', { name: 'Request approval' }));

    await waitFor(() => expect(changed).toBe(1));
    expect(calls.relations).toBe(1);
    expect(calls.approvals).toEqual([]);
  });

  it('attaches at once, with no dialog, when the agent brings nothing', async () => {
    const calls = serve({ preview: preview({ items: [], approvable: [] }) });
    let changed = 0;
    renderMenu(() => (changed += 1));

    await chooseOtherBot();

    await waitFor(() => expect(changed).toBe(1));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(calls.relations).toBe(1);
    expect(calls.approvals).toEqual([]);
  });

  it('attaches anyway when the review cannot be read, leaving the grant pending for the row to show', async () => {
    const calls = serve({ preview: 'unreadable' });
    let changed = 0;
    renderMenu(() => (changed += 1));

    await chooseOtherBot();

    await waitFor(() => expect(changed).toBe(1));
    expect(calls.relations).toBe(1);
    expect(calls.approvals).toEqual([]);
  });

  it('keeps the attach and says why when the approval fails', async () => {
    const calls = serve({ preview: preview(), approve: 'fail' });
    let changed = 0;
    renderMenu(() => (changed += 1));

    await chooseOtherBot();
    fireEvent.click(await screen.findByRole('button', { name: 'Approve and attach' }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Added, but the delegation is not approved: nothing is left to approve');
    expect(alert).toHaveTextContent('Review delegation');
    expect(calls.relations).toBe(1);
    expect(changed).toBe(1);
  });
});
