import { Blob as NodeBlob } from 'node:buffer';
import { webcrypto } from 'node:crypto';
import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { MessageFeedbackSummary, MessageTraceStepDetail } from '@/shared/api/generated/model';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { ChatBox } from '@/widgets/chat-box';
import { toParticipant } from '@/widgets/chat-box/ui/ChatBox.helpers';
import { PersistedMessageTrace } from '@/features/chat-messages/ui/chat-box/PersistedMessageTrace';
import { server } from '@/test/setup';

import replay from '../__tests__/fixtures/code-debug-live-replay.jsonl?raw';
import snapshot from '../__tests__/fixtures/code-debug-live-snapshot.json?raw';
import { renderPipelinesRoute } from '../__tests__/testRouter';
import { resetPipelineTestConversationsForTests } from '../lib/usePipelineTestConversation';
import { PipelineTestChat } from './PipelineTestChat';

installCodeMirrorTestPolyfills();
const frames = replay.trim().split('\n').map(line => JSON.parse(line) as Record<string, unknown>);
const start = frames[0]!;
const entry = frames[1]!['response_metadata'] as Record<string, unknown>;
const finalText = frames.at(-1)!['content'] as string;
const question = 'code-debug-generation-ui-20261005';
const executionId = 'cf9ad06c51f3951e37a0ec38eeaa1ea2';
const artifactName = 'b90f6f0bea803692a23ea0db342d579f069664d178500056cf0edbeda8e13d09.json';
const user = { id: '6', name: 'Code test user', avatar: '' };
const participant = { id: '901', entity_name: 'application', entity_meta: { id: '143', project_id: '2' },
  entity_settings: { version_id: '164', agent_type: 'pipeline' } };
const conversation = { id: 501, uuid: start['stream_id'] as string, name: 'Code pipeline', source: 'conversation', is_private: true,
  participants: [{ id: '900', entity_name: 'user', entity_meta: { id: 6 } }, participant] };
let registry: TestEventSourceRegistry;
let admissions: number;
let artifactReads: number;

beforeEach(() => {
  registry = installTestEventSource();
  admissions = 0;
  artifactReads = 0;
  vi.stubGlobal('Blob', NodeBlob);
  vi.stubGlobal('crypto', webcrypto);
  vi.stubGlobal('elitea_ui_config', { vite_server_url: '/api/v2', vite_base_uri: '/', vite_public_project_id: 'public' });
  Object.defineProperty(Element.prototype, 'scrollIntoView', { configurable: true, writable: true, value: vi.fn() });
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: '/api/v2' });
  resetPipelineTestConversationsForTests();
  server.use(
    http.post('*/elitea_core/conversations/prompt_lib/2', () => HttpResponse.json({ ...conversation, source: 'editor_test',
      meta: { is_hidden: true, editor_test: { revision: 1, actor_id: '6', project_id: '2', application_id: '143', application_version_id: '164' } } })),
    http.get('*/elitea_core/conversation/prompt_lib/2/:id', () => HttpResponse.json(conversation)),
    http.post('*/elitea_core/messages/prompt_lib/2/:uuid', async ({ request, params }) => {
      expect(params['uuid']).toBe(start['stream_id']);
      expect(await request.json()).toMatchObject({ payload: { user_input: question } });
      admissions++;
      return HttpResponse.json({ execution_id: executionId, response_message_id: start['message_id'],
        execution_generation: start['execution_generation'], events_url: `/api/v2/executions/2/${executionId}/events` });
    }),
    http.get('*/configurations/models/2', () => HttpResponse.json({ items: [{ name: 'fixture-model', project_id: '2', default: true }], default_model_name: 'fixture-model' })),
    http.get('*/configurations/tts_voices/*', () => HttpResponse.json({ items: [] })),
    http.get('*/elitea_core/application/prompt_lib/2/143', () => HttpResponse.json({ id: '143', name: 'Code pipeline',
      versions: [{ id: '164', name: 'Saved version' }], version_details: { id: '164', agent_type: 'pipeline' } })),
    http.get('*/elitea_core/message_traces/prompt_lib/2/:id', () => HttpResponse.json({ rows: [], limit: 50, offset: 0, has_more: false })),
    http.get(`/api/v2/elitea_core/message_feedback/prompt_lib/2/${String(start['message_id'])}`, () => HttpResponse.json<MessageFeedbackSummary>({ likes: 0, dislikes: 0 })),
    http.get(`/api/v2/artifacts/objects/2/code-debug/${artifactName}`, ({ request }) => {
      artifactReads++;
      expect(request.credentials).toBe('same-origin');
      expect(request.headers.get('authorization')).toBeNull();
      return new HttpResponse(snapshot, { headers: { 'Content-Type': 'application/json' } });
    }),
  );
});
afterEach(() => {
  registry.restore();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  resetConfigForTests();
  resetGeneratedClient();
});

function renderChat(mode: 'editor' | 'main' | 'reload') {
  const groups = mode === 'reload' ? [{ id: 51, uuid: start['message_id'] as string, author_participant_id: '901', role: 'assistant',
    created_at: start['created_at'] as string, content: finalText, is_streaming: false, meta: {},
    message_items: [{ id: 1, item_details: { content: finalText } }] }] : [];
  return renderPipelinesRoute(mode === 'editor'
    ? <PipelineTestChat settings={{}} disableChat={false} slotRef={undefined} user={user}
      identity={{ projectId: '2', applicationId: '143', pipelineName: 'Code pipeline', versionId: '164', agentType: 'pipeline' }} />
    : <ChatBox projectId="2" conversation={{ active: { ...conversation, message_groups: groups } }}
      participant={{ active: toParticipant(participant) }} user={user} />,
  '/pipelines/all/143', { projectId: '2' });
}

async function openGroupedExport(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: /Thought for/ }));
  await user.click(screen.getByRole('button', { name: /^debug_probe$/i }));
  await user.click(screen.getByText('debug_probe / debug export'));
  expect(await screen.findByRole('dialog')).toHaveTextContent('Code debug · debug_probe · attempt 1');
  expect(artifactReads).toBe(0);
}

async function verifyDownload(user: ReturnType<typeof userEvent.setup>) {
  const createUrl = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:verified-code-debug');
  const revokeUrl = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
  const anchorClick = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
  await user.click(screen.getByRole('button', { name: 'Download verified snapshot' }));
  await waitFor(() => expect(createUrl).toHaveBeenCalledTimes(1));
  expect(artifactReads).toBe(1);
  expect(anchorClick).toHaveBeenCalledTimes(1);
  expect(revokeUrl).toHaveBeenCalledWith('blob:verified-code-debug');
  const blob = createUrl.mock.calls[0]?.[0];
  if (!(blob instanceof Blob)) throw new Error('Expected a verified snapshot Blob');
  expect(await blob.text()).toBe(snapshot);
}

it.each(['editor', 'main'] as const)('opens and downloads the exact live replay receipt through %s chat', async mode => {
  const user = userEvent.setup();
  renderChat(mode);
  const input = await screen.findByTestId('chat-message-input');
  await user.click(input);
  expect(input).toHaveFocus();
  await user.keyboard('c');
  expect(input).toHaveValue('c');
  await user.paste(question.slice(1));
  expect(input).toHaveValue(question);
  await waitFor(() => expect(screen.getByTestId('chat-send-button')).toBeEnabled());
  await user.keyboard('{Enter}');
  await waitFor(() => expect(registry.getOpen()).toHaveLength(1));
  expect(registry.getOpen()[0]?.url).toBe(`/api/v2/executions/2/${executionId}/events`);
  act(() => frames.forEach((frame, index) => registry.emit('execution.node_event', JSON.stringify(frame), String(index + 1))));
  await waitFor(() => expect(registry.getOpen()).toHaveLength(0));
  expect(admissions).toBe(1);
  await openGroupedExport(user);
  await verifyDownload(user);
});

it('opens the same grouped receipt after persisted trace restoration without admitting a new run', async () => {
  const user = userEvent.setup();
  server.use(http.get('*/elitea_core/message_traces/prompt_lib/2/:id', () => HttpResponse.json({ rows: [
    { id: 101, message_group_id: 51, kind: 'tool_call', tool_name: entry['tool_name'],
      is_error: false,
      started_at: entry['timestamp_start'], finished_at: entry['timestamp_finish'],
      attrs: { metadata: entry['metadata'], tool_meta: entry['tool_meta'] } },
  ], limit: 50, offset: 0, has_more: false })),
  http.get('/api/v2/elitea_core/message_trace/prompt_lib/2/101', ({ request }) => {
    expect(new URL(request.url).searchParams.get('message_group_id')).toBe('51');
    return HttpResponse.json<MessageTraceStepDetail>({ id: 101, message_group_id: 51, kind: 'tool_call', is_error: false,
      tool_name: String(entry['tool_name']),
      attrs: entry, tool_inputs: {}, tool_output: null });
  }));
  renderChat('reload');
  await openGroupedExport(user);
  await verifyDownload(user);
  expect(admissions).toBe(0);
});

it('downloads the same receipt through the persisted trace detail fallback', async () => {
  const user = userEvent.setup();
  const step = { id: 101, message_group_id: 51, kind: 'tool_call', tool_name: String(entry['tool_name']), is_error: false };
  server.use(http.get('/api/v2/elitea_core/message_trace/prompt_lib/2/101', ({ request }) => {
    expect(new URL(request.url).searchParams.get('message_group_id')).toBe('51');
    return HttpResponse.json<MessageTraceStepDetail>({ ...step, attrs: entry, tool_inputs: {}, tool_output: null });
  }));
  renderPipelinesRoute(<PersistedMessageTrace value={{ projectId: '2', conversationId: '501', messageGroupId: 51,
    steps: [step], failed: false }} />, '/pipelines/all/143', { projectId: '2' });
  await user.click(await screen.findByRole('button', { name: 'Execution details' }));
  await user.click(screen.getByRole('button', { name: 'debug_probe / debug export' }));
  expect(await screen.findByText('Code debug · debug_probe · attempt 1')).toBeVisible();
  expect(artifactReads).toBe(0);
  await verifyDownload(user);
  expect(admissions).toBe(0);
});
