/**
 * ADR-0050 D7c through the REAL `ChatBox` (issue 1625): in a conversation with
 * two agents and nothing picked, an unaddressed message goes to the agent the
 * history last addressed. Three consumers need that target: the start body
 * (`participant_id`), the stream reducer's identity (the answer's
 * `participantId`) and the attachments gate (whose version it reads). The
 * pure helpers are tested on their own; this pins the JOIN in the composition
 * root, where dropping the target from one consumer used to pass every test.
 *
 * Only the network is substituted: MSW for REST, the `EventSource` double for
 * the stream, the in-memory socket client.
 */
import type { ReactElement } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';

import type { MessageGroupWire } from '@/entities/message';
import type { ChatMessage } from '@/features/chat-messages';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';

import ChatBox from './ChatBox';

const INTEGRATION_TIMEOUT_MS = 20_000;
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
const UUID = '00000000-0000-4000-8000-000000001625';
const EVENTS_URL = '/api/v2/executions/2/exec-1625/events';
const RESPONSE_ID = '00000000-0000-4000-8000-0000000016a0';
const GENERATION = '00000000-0000-4000-8000-0000000016b0';

const USER = { id: '900', entity_name: 'user', entity_meta: { id: 6 }, meta: { user_name: 'Reader' } };
const AGENT_A = { id: '901', entity_name: 'application', entity_meta: { id: 143, project_id: '2', name: 'Agent A' }, entity_settings: { version_id: '164' } };
const AGENT_B = { id: '902', entity_name: 'application', entity_meta: { id: 144, project_id: '2', name: 'Agent B' }, entity_settings: { version_id: '165' } };

/** A asked first, then B: the history last addressed B. */
function group(id: number, fields: Partial<MessageGroupWire> & { readonly content: string; readonly role: string }): MessageGroupWire {
  return { id, uuid: `00000000-0000-4000-8000-00000000${String(id).padStart(4, '0')}`, created_at: `2026-11-09T10:0${id}:00Z`, meta: {},
    message_items: [{ id, item_details: { content: fields.content } }], ...fields };
}
const HISTORY = [
  group(1, { role: 'user', author_participant_id: '900', sent_to_id: '901', content: 'hi A' }),
  group(2, { role: 'assistant', author_participant_id: '901', reply_to_id: '900', content: 'A here' }),
  group(3, { role: 'user', author_participant_id: '900', sent_to_id: '902', content: 'hi B' }),
  group(4, { role: 'assistant', author_participant_id: '902', reply_to_id: '900', content: 'B here' }),
];
const CONVERSATION = { id: 1625, uuid: UUID, name: 'Two agents', participants: [USER, AGENT_A, AGENT_B], message_groups: HISTORY };

/**
 * B's version allows attachments, A's does not; until details arrive the gate reads OFF, so ENABLED says it read B's.
 * Each version has its own toolkit tool, so the tools "Run to completion" offers say whose version they came from.
 */
function application(id: number, versionId: string, internalTools: readonly string[], toolkit: string) {
  return { id, name: `Agent ${String(id)}`, versions: [{ id: versionId, name: 'base' }],
    version_details: { id: versionId, meta: { internal_tools: internalTools }, tools: [{ toolkit_name: toolkit, selected_tools: ['merge'] }] } };
}

let registry: TestEventSourceRegistry;
let applicationReads: string[];
let startBodies: Record<string, unknown>[];
let history: readonly ChatMessage[];
let scrollIntoView: PropertyDescriptor | undefined;

beforeEach(() => {
  registry = installTestEventSource();
  applicationReads = [];
  startBodies = [];
  history = [];
  vi.stubGlobal('elitea_ui_config', { vite_server_url: '/api/v2', vite_base_uri: '/', vite_public_project_id: 'public' });
  scrollIntoView = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollIntoView');
  Object.defineProperty(Element.prototype, 'scrollIntoView', { configurable: true, writable: true, value: vi.fn() });
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/elitea_core/application/prompt_lib/2/:id', ({ params }) => {
      applicationReads.push(String(params['id']));
      return HttpResponse.json(params['id'] === '143' ? application(143, '164', [], 'toolkit_a') : application(144, '165', ['attachments'], 'toolkit_b'));
    }),
    http.post(`*/elitea_core/messages/prompt_lib/2/${UUID}`, async ({ request }) => {
      startBodies.push(await request.json() as Record<string, unknown>);
      return HttpResponse.json({ execution_id: 'exec-1625', response_message_id: RESPONSE_ID, execution_generation: GENERATION, events_url: EVENTS_URL });
    }),
    http.get('*/configurations/models/2', () => HttpResponse.json({ items: [{ name: 'fixture-model', project_id: '2', default: true }], default_model_name: 'fixture-model' })),
    http.get('*/configurations/tts_voices/*', () => HttpResponse.json({ items: [] })),
    http.get('*/elitea_core/message_traces/prompt_lib/2/:id', () => HttpResponse.json({ rows: [], limit: 50, offset: 0, has_more: false })),
    http.get('*/elitea_core/message_feedback/prompt_lib/2/:id', () => HttpResponse.json({ likes: 0, dislikes: 0 })),
  );
});

afterEach(() => {
  if (scrollIntoView === undefined) Reflect.deleteProperty(Element.prototype, 'scrollIntoView');
  else Object.defineProperty(Element.prototype, 'scrollIntoView', scrollIntoView);
  registry.restore();
  vi.unstubAllGlobals();
  resetConfigForTests();
  resetGeneratedClient();
});

function renderChat(): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } } });
  const socketClient = createTestSocketClient();
  function Root(): ReactElement {
    return (
      <QueryClientProvider client={queryClient}>
        <SocketClientContext.Provider value={socketClient}>
          <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
            {/* No `participant`: nothing is picked, so the history decides. */}
            <ChatBox projectId="2" conversation={{ active: CONVERSATION }} user={{ id: '6', name: 'Reader', avatar: '' }}
              setChatHistory={(next) => { if (typeof next !== 'function') history = next as readonly ChatMessage[]; }} />
          </ThemeProvider>
        </SocketClientContext.Provider>
      </QueryClientProvider>
    );
  }
  const router = createRouter({
    routeTree: createRootRoute({ component: Root }),
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => '2' } },
  });
  render(<RouterProvider router={router} />);
}

it('sends to, attributes the answer to, and gates attachments on the agent the history last addressed', async () => {
  const user = userEvent.setup();
  renderChat();

  // "To:" names B.
  expect(await screen.findByTestId('chat-floor-target')).toHaveTextContent('Agent B');

  // The attachments gate read B's version (attachments on), never A's.
  await waitFor(() => expect(applicationReads).toContain('144'));
  await user.click(screen.getByTestId('plus-menu-button'));
  await waitFor(() => expect(screen.getByTestId('plus-menu-attachments')).toHaveAttribute('aria-disabled', 'false'));
  expect(applicationReads).not.toContain('143');
  await user.keyboard('{Escape}');

  // "Run to completion" offers B's tools: the grant names tools of the agent the send goes to.
  await user.click(await screen.findByTestId('chat-run-to-completion-open'));
  expect(await screen.findByTestId('unattended-sensitive-tools')).toHaveTextContent('toolkit_b/merge');
  expect(screen.getByTestId('unattended-sensitive-tools')).not.toHaveTextContent('toolkit_a');
  await user.keyboard('{Escape}');

  // The start request carries B's participant id.
  const input = await screen.findByTestId('chat-message-input');
  await user.click(input);
  await user.keyboard('who answers?');
  await waitFor(() => expect(screen.getByTestId('chat-send-button')).toBeEnabled());
  await user.keyboard('{Enter}');
  await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
  expect(startBodies).toHaveLength(1);
  expect(startBodies[0]).toMatchObject({ participant_id: 902 });

  // The stream reducer attributes a frame that names no participant to B.
  act(() => {
    registry.emit('execution.node_event', JSON.stringify({ type: 'agent_start', stream_id: UUID, message_id: RESPONSE_ID,
      execution_generation: GENERATION, response_metadata: {}, created_at: '2026-11-09T10:10:00Z' }), '1');
  });
  await waitFor(() => expect(history.find((message) => message.id === RESPONSE_ID)?.participantId).toBe('902'));
  // Nothing in the turn (the optimistic question, the streamed answer) moved the gate to A.
  expect(applicationReads).not.toContain('143');
}, INTEGRATION_TIMEOUT_MS);
