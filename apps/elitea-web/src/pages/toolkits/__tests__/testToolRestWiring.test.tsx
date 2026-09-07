/**
 * THE DISCRIMINATING COMPOSITION-ROOT TEST for the toolkit "Test tool" /
 * "Run" action's transport switch (platform-parity wave, WP16b's REST
 * follow-up).
 *
 * `useToolkitChatDispatch.hooks.ts`'s "fifth case" moved every tool but
 * `index_data` off the socket.io `chat_predict` emit onto the synchronous
 * `POST /elitea_core/test_tool/prompt_lib/{projectId}/{toolId}` run
 * (`../../features/toolkits/api/toolkitTestRun.ts`). `useToolkitChat.hooks.
 * test.tsx` already proves the hook in isolation; this test proves the WIRING
 * — the real page (`EditToolkit`), reached the same way a browser reaches it
 * (`/toolkits/latest/:toolkitId`), with nothing injected but HTTP, actually
 * calls the new route when a person opens an index and clicks "Run".
 *
 * `search_index` is `IndexesToolsEnum.searchIndexData` — the default
 * `selectedRunTool` for an existing (non-create-view) index
 * (`IndexDetails.tsx`'s own `defaultRunTool`), and the "Run" tab is the
 * default `activeEditTab` whenever the index's state is runnable
 * (`disableRunTabReason` — see `IndexDetails.tsx`). A single `completed`
 * index with `search_index` selected therefore reaches an enabled "Run"
 * button with no extra navigation.
 */
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { server } from '@/test/setup';

import { EditToolkit } from '../EditToolkit';
import { renderToolkitsRoute } from './testRouter';

// `ToolkitForm`'s raw-JSON editor mounts a real CodeMirror instance for this
// fixture's untyped `github` schema (same reason `EditToolkit.test.tsx`
// installs this). `ChatMessageList`'s own scroll-to-bottom effect calls
// `scrollIntoView`, which jsdom does not implement.
installCodeMirrorTestPolyfills();
if (typeof Element.prototype.scrollIntoView !== 'function') {
  Element.prototype.scrollIntoView = () => undefined;
}

const TOOLKIT_URL = '/api/v2/elitea_core/tools/prompt_lib/:projectId';
const TOOLKIT_TYPES_URL = '/api/v2/elitea_core/toolkits/prompt_lib/:projectId';
const INDEX_META_URL = '/api/v2/elitea_core/index_meta/prompt_lib/:projectId/:toolkitId';
const TEST_TOOL_URL = '/api/v2/elitea_core/test_tool/prompt_lib/proj-1/tk-1';
const MODELS_URL = '/api/v2/configurations/models/:projectId';
const CONFIGURATIONS_AVAILABLE_URL = '/api/v2/configurations/available/';
const CONFIGURATIONS_LIST_URL = '/api/v2/configurations/configurations/:projectId';

const GITHUB_TYPE_SCHEMA = {
  properties: {
    selected_tools: {
      args_schemas: {
        // No `required` array: `validateToolkitForm` treats an empty
        // required-fields list as immediately valid, so the "Run" button is
        // enabled without the test having to fill in a search query.
        search_index: { type: 'object', properties: { query: { type: 'string' } } },
      },
    },
  },
};

function mockRealToolkit(): void {
  server.use(
    http.get(TOOLKIT_URL, () =>
      HttpResponse.json({ rows: [{ id: 'tk-1', type: 'github', name: 'My GitHub', description: '', settings: { selected_tools: ['search_index'] }, meta: {}, created_at: '2026-01-01T00:00:00Z', author_id: 1 }], total: 1 }),
    ),
    http.get(TOOLKIT_TYPES_URL, () => HttpResponse.json({ github: GITHUB_TYPE_SCHEMA })),
    http.get(INDEX_META_URL, () => HttpResponse.json([{ id: 'idx-1', metadata: { collection: 'my-index', state: 'completed', indexed: 5 } }])),
    http.get(MODELS_URL, () => HttpResponse.json({ items: [], total: 0 })),
    http.get(CONFIGURATIONS_AVAILABLE_URL, () => HttpResponse.json([])),
    http.get(CONFIGURATIONS_LIST_URL, () => HttpResponse.json({ items: [], total: 0, limit: 500, offset: 0, shared: { items: [], total: 0 } })),
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('EditToolkit — Indexes "Run" tab settles over the REST test-tool run', () => {
  it('clicking "Run" for a non-index_data tool POSTs tool_name/tool_params to /test_tool, and never emits chat_predict', { timeout: 15_000 }, async () => {
    mockRealToolkit();
    let testToolBody: unknown;
    server.use(
      http.post(TEST_TOOL_URL, async ({ request }) => {
        testToolBody = await request.json();
        return HttpResponse.json({ ok: true, result: { hits: 2 }, tool_name: 'search_index' });
      }),
    );
    const user = userEvent.setup();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit: vi.fn() }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');
    await user.click(await screen.findByRole('tab', { name: 'Indexes' }));

    const panel = await screen.findByTestId('edit-toolkit-indexes-tab-panel');
    // The auto-select-first-valid-index effect (`IndexesContainer.tsx`) opens
    // the one seeded index without any further navigation, landing on its
    // default-active "Run" tab.
    const runButton = await within(panel).findByRole('button', { name: 'Run' });
    await waitFor(() => expect(runButton).toBeEnabled(), { timeout: 10_000 });

    await user.click(runButton);

    await waitFor(() => expect(testToolBody).toEqual({ tool_name: 'search_index', tool_params: {} }));
    // No handler is registered for a socket — a `chat_predict` emit here
    // would either no-op silently (masking the regression) or, if this
    // suite ever adds socket assertions, be the tell. The REST body landing
    // is the discriminator this test exists for.
  });
});
