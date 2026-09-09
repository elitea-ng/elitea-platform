/**
 * Coverage for `LongTermMemoryManagement` (#870) — Settings > Memory's
 * "Long-term Memory" panel, the real replacement for
 * `ui/profile/ProfileLongTermMemory.tsx`'s old "Coming soon" placeholder.
 * Same MSW harness `pages/settings/Webhooks.test.tsx` uses for the sibling
 * settings tab.
 */
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { LongTermMemoryManagement } from './LongTermMemoryManagement';

const BASE = '/api/v2';
const PROJECT_ID = 'proj-1';
const PERMISSIONS_PATH = `${BASE}/auth/permissions/prompt_lib/:projectId`;
const MEMORIES_PATH = `${BASE}/elitea_core/memories/prompt_lib/:projectID`;
const MEMORY_PATH = `${BASE}/elitea_core/memory/prompt_lib/:projectID/:memoryID`;

function fullPermissions(): ReturnType<typeof http.get> {
  return http.get(PERMISSIONS_PATH, () =>
    HttpResponse.json([
      { name: 'models.chat.conversation.details', enabled: true },
      { name: 'models.chat.conversation.update', enabled: true },
    ]),
  );
}

function readOnlyPermissions(): ReturnType<typeof http.get> {
  return http.get(PERMISSIONS_PATH, () => HttpResponse.json([{ name: 'models.chat.conversation.details', enabled: true }]));
}

function memoryEntry(overrides: Partial<Record<string, unknown>> = {}) {
  return {
    id: 'mem-1',
    project_id: PROJECT_ID,
    content: 'Prefers TypeScript over Python for new services.',
    tags: ['preferences'],
    enabled: true,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

function mount() {
  return render(
    <AppProviders>
      <LongTermMemoryManagement projectId={PROJECT_ID} />
    </AppProviders>,
  );
}

describe('LongTermMemoryManagement — happy path', () => {
  it('renders the fetched memories as rows', async () => {
    server.use(fullPermissions(), http.get(MEMORIES_PATH, () => HttpResponse.json({ items: [memoryEntry()], total: 1 })));

    mount();

    await waitFor(() => {
      expect(screen.getByText(/Prefers TypeScript over Python/)).toBeInTheDocument();
    });
    expect(screen.getByText('preferences')).toBeInTheDocument();
  });

  it('shows an empty state for a project with no memories', async () => {
    server.use(fullPermissions(), http.get(MEMORIES_PATH, () => HttpResponse.json({ items: [], total: 0 })));

    mount();

    await waitFor(() => {
      expect(screen.getByTestId('long-term-memory-empty')).toBeInTheDocument();
    });
  });

  it('creates a memory via the add dialog', async () => {
    let created: unknown = null;
    server.use(
      fullPermissions(),
      http.get(MEMORIES_PATH, () => HttpResponse.json({ items: [], total: 0 })),
      http.post(MEMORIES_PATH, async ({ request }) => {
        created = await request.json();
        return HttpResponse.json(memoryEntry({ content: 'A new memory' }), { status: 201 });
      }),
    );

    mount();

    // `long-term-memory-add` renders only once `canWrite` resolves — waiting
    // for it (rather than the list's own empty state, which resolves off a
    // SEPARATE query) is the deterministic "permissions AND list are both
    // ready" signal every write-flow test below needs.
    await waitFor(() => expect(screen.getByTestId('long-term-memory-add')).toBeEnabled());
    fireEvent.click(screen.getByTestId('long-term-memory-add'));

    const dialog = await screen.findByTestId('long-term-memory-form-dialog');
    const contentField = within(dialog).getByTestId('long-term-memory-form-content').querySelector('textarea, input');
    if (!contentField) throw new Error('content field not found');
    fireEvent.change(contentField, { target: { value: 'A new memory' } });

    fireEvent.click(within(dialog).getByText('Create'));

    await waitFor(() => expect(created).toMatchObject({ content: 'A new memory', enabled: true }));
  });

  it('refuses an empty memory before it reaches the server', async () => {
    let createCalls = 0;
    server.use(
      fullPermissions(),
      http.get(MEMORIES_PATH, () => HttpResponse.json({ items: [], total: 0 })),
      http.post(MEMORIES_PATH, () => {
        createCalls += 1;
        return HttpResponse.json(memoryEntry(), { status: 201 });
      }),
    );

    mount();

    await waitFor(() => expect(screen.getByTestId('long-term-memory-add')).toBeEnabled());
    fireEvent.click(screen.getByTestId('long-term-memory-add'));
    const dialog = await screen.findByTestId('long-term-memory-form-dialog');
    fireEvent.click(within(dialog).getByText('Create'));

    expect(await within(dialog).findByText(/needs some text/i)).toBeInTheDocument();
    expect(createCalls).toBe(0);
  });

  it('toggles a memory enabled via the row Switch', async () => {
    let updateBody: unknown = null;
    server.use(
      fullPermissions(),
      http.get(MEMORIES_PATH, () => HttpResponse.json({ items: [memoryEntry({ enabled: true })], total: 1 })),
      http.put(MEMORY_PATH, async ({ request }) => {
        updateBody = await request.json();
        return HttpResponse.json(memoryEntry({ enabled: false }));
      }),
    );

    mount();

    // Wait for `canWrite` to resolve (the add button's own gate) before
    // clicking the row Switch — it renders immediately but stays `disabled`
    // until the SAME permission query settles, and a click on a disabled
    // MUI Switch is a silent no-op, not a failure.
    await waitFor(() => expect(screen.getByTestId('long-term-memory-add')).toBeEnabled());
    // Unlike the master toggle (wrapped in a `<label>` via
    // `FormControlLabel`, where a click anywhere inside triggers the
    // associated control even in jsdom), a row's bare `<Switch>` has no
    // label wrapper — `fireEvent.click` on its outer span never reaches the
    // real `<input>`. Click the input itself.
    const toggleInput = screen.getByTestId('long-term-memory-enabled-toggle-mem-1').querySelector('input');
    if (!toggleInput) throw new Error('toggle input not found');
    fireEvent.click(toggleInput);

    await waitFor(() => expect(updateBody).toMatchObject({ enabled: false }));
  });

  it('deletes a memory after confirmation', async () => {
    let deleteCalls = 0;
    server.use(
      fullPermissions(),
      http.get(MEMORIES_PATH, () => HttpResponse.json({ items: [memoryEntry()], total: 1 })),
      http.delete(MEMORY_PATH, () => {
        deleteCalls += 1;
        return new HttpResponse(null, { status: 204 });
      }),
    );

    mount();

    await waitFor(() => expect(screen.getByTestId('long-term-memory-delete-mem-1')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('long-term-memory-delete-mem-1'));

    const confirmDialog = await screen.findByTestId('long-term-memory-delete-confirm-dialog');
    fireEvent.click(within(confirmDialog).getByText('Delete'));

    await waitFor(() => expect(deleteCalls).toBe(1));
  });

  it('clears all memories after the clear-all confirmation', async () => {
    let clearCalls = 0;
    server.use(
      fullPermissions(),
      http.get(MEMORIES_PATH, () => HttpResponse.json({ items: [memoryEntry()], total: 1 })),
      http.delete(MEMORIES_PATH, () => {
        clearCalls += 1;
        return HttpResponse.json({ removed: 1 });
      }),
    );

    mount();

    await waitFor(() => expect(screen.getByTestId('long-term-memory-clear-all')).toBeEnabled());
    fireEvent.click(screen.getByTestId('long-term-memory-clear-all'));

    const confirmDialog = await screen.findByTestId('long-term-memory-clear-all-confirm-dialog');
    fireEvent.click(within(confirmDialog).getByText('Clear all'));

    await waitFor(() => expect(clearCalls).toBe(1));
  });

  it('the master toggle bulk-disables every enabled memory', async () => {
    const putCalls: string[] = [];
    server.use(
      fullPermissions(),
      http.get(MEMORIES_PATH, () =>
        HttpResponse.json({
          items: [memoryEntry({ id: 'mem-1', enabled: true }), memoryEntry({ id: 'mem-2', enabled: true })],
          total: 2,
        }),
      ),
      http.put(MEMORY_PATH, ({ params }) => {
        putCalls.push(String(params['memoryID']));
        return HttpResponse.json(memoryEntry({ id: String(params['memoryID']), enabled: false }));
      }),
    );

    mount();

    await waitFor(() => expect(screen.getByTestId('long-term-memory-add')).toBeEnabled());
    fireEvent.click(screen.getByTestId('long-term-memory-master-toggle'));

    await waitFor(() => expect(putCalls.sort()).toEqual(['mem-1', 'mem-2']));
  });

  it('filters the list by search text', async () => {
    let lastQuery: string | null = null;
    server.use(
      fullPermissions(),
      http.get(MEMORIES_PATH, ({ request }) => {
        lastQuery = new URL(request.url).searchParams.get('q');
        return HttpResponse.json({ items: [memoryEntry()], total: 1 });
      }),
    );

    mount();
    await waitFor(() => expect(screen.getByTestId('long-term-memory-search')).toBeInTheDocument());

    const searchInput = screen.getByTestId('long-term-memory-search').querySelector('input');
    if (!searchInput) throw new Error('search input not found');
    fireEvent.change(searchInput, { target: { value: 'typescript' } });

    await waitFor(() => expect(lastQuery).toBe('typescript'));
  });
});

describe('LongTermMemoryManagement — read-only permissions', () => {
  it('hides every write control when the caller cannot write', async () => {
    server.use(readOnlyPermissions(), http.get(MEMORIES_PATH, () => HttpResponse.json({ items: [memoryEntry()], total: 1 })));

    mount();

    await waitFor(() => expect(screen.getByText(/Prefers TypeScript/)).toBeInTheDocument());
    expect(screen.queryByTestId('long-term-memory-add')).not.toBeInTheDocument();
    expect(screen.queryByTestId('long-term-memory-clear-all')).not.toBeInTheDocument();
    expect(screen.queryByTestId('long-term-memory-edit-mem-1')).not.toBeInTheDocument();
    expect(screen.queryByTestId('long-term-memory-delete-mem-1')).not.toBeInTheDocument();
  });
});
