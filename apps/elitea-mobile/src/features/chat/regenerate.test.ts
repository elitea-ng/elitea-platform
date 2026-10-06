import {
  CAPTURE,
  REGENERATION_ID,
  answerGroupAfterRegenerate,
  answerGroupBefore,
  questionGroup,
  regeneratePending,
  regenerateStarted,
} from '@/api/__fixtures__/chat13';
import { createApiClient } from '@/api/client';
import type { BaseFetch } from '@/api/middleware';
import { REGENERATE_CONTRACT, TurnInputError, prepareRegeneration } from '@/api/queries/sendMessage';
import { loadFixtureText } from '@/features/chat/__fixtures__/loadFixture';
import { answerRegenerated, toChatMessages } from '@/features/chat/messageModel';
import { TurnController, answerHiddenByRegeneration, type LiveTurn } from '@/features/chat/turnController';
import { abortAll } from '@/lib/abortRegistry';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN } from '@/test/fakeServer';
import { fakeStreamTransport } from '@/test/fakeStreamTransport';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-crypto', () => ({ randomUUID: jest.fn(() => 'F59416E1-C2EF-436A-8108-674C616D3DCF') }));

const P = CAPTURE.projectId;
const target = {
  projectId: P,
  conversationUuid: CAPTURE.conversationUuid,
  participantId: CAPTURE.agentParticipantId,
  questionUuid: CAPTURE.questionUuid,
  answerUuid: CAPTURE.answerUuid,
  questionText: questionGroup.content,
};
const flush = async () => {
  for (let i = 0; i < 10; i++) await new Promise<void>((r) => setTimeout(r, 0));
};

beforeEach(() => {
  __resetSessionForTests();
  abortAll();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'debug').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('regeneration body', () => {
  it('is the body the captured server admitted: message_id = stream_id = the answer, question_id = the question group', () => {
    const { bodyJson } = prepareRegeneration({ ...target, regenerationId: REGENERATION_ID });
    expect(JSON.parse(bodyJson)).toEqual({
      payload: { user_input: questionGroup.content },
      project_id: P,
      participant_id: CAPTURE.agentParticipantId,
      conversation_uuid: CAPTURE.conversationUuid,
      question_id: CAPTURE.questionUuid,
      message_id: CAPTURE.answerUuid,
      stream_id: CAPTURE.answerUuid,
      regeneration_id: REGENERATION_ID,
    });
  });

  it('a model conversation sends the same llm_settings as its send; an empty question still sends text', () => {
    const { bodyJson } = prepareRegeneration({
      ...target,
      questionText: '  ',
      participantId: 0,
      model: { name: 'vllm/E2E-MOCK-MODEL', projectId: 1 },
      regenerationId: REGENERATION_ID,
    });
    expect(JSON.parse(bodyJson).payload).toEqual({
      user_input: '(regenerate)',
      llm_settings: { model_name: 'vllm/E2E-MOCK-MODEL', model_project_id: 1, stream: true },
    });
  });

  it('refuses ids that are not uuids', () => {
    expect(() => prepareRegeneration({ ...target, answerUuid: 'g6', regenerationId: REGENERATION_ID })).toThrow(TurnInputError);
    expect(() => prepareRegeneration({ ...target, regenerationId: 'NOT-LOWER' })).toThrow(TurnInputError);
  });
});

describe('a regeneration replaces the answer (captured on the zefir-wave stack)', () => {
  it('the rewritten answer keeps its uuid, gets a new item and carries the regeneration id as its generation', () => {
    expect(answerGroupAfterRegenerate.uuid).toBe(answerGroupBefore.uuid);
    expect(answerGroupAfterRegenerate.task_id).not.toBe(answerGroupBefore.task_id);
    expect(answerRegenerated({ message_groups: [questionGroup, answerGroupBefore] }, CAPTURE.answerUuid, REGENERATION_ID)).toBe(false);
    expect(answerRegenerated({ message_groups: [questionGroup, answerGroupAfterRegenerate] }, CAPTURE.answerUuid, REGENERATION_ID)).toBe(true);
    // The first answer's generation is its question's id.
    const [, first] = toChatMessages({ participants: [], message_groups: [questionGroup, answerGroupBefore] });
    expect(first.generation).toBe(CAPTURE.questionUuid);
    expect(first.replyToGroupId).toBe(questionGroup.id);
  });

  it('POSTs once more after 409 agent_regeneration_pending (same bytes), then streams the new answer', async () => {
    const posts: { url: string; body: string }[] = [];
    const answers = [
      () => new Response(JSON.stringify(regeneratePending), { status: 409, headers: { 'Content-Type': 'application/json', 'Retry-After': '1' } }),
      () => new Response(JSON.stringify(regenerateStarted), { status: 200, headers: { 'Content-Type': 'application/json' } }),
    ];
    const fetch: BaseFetch = async (input) => {
      posts.push({ url: input.url, body: await input.clone().text() });
      return answers[Math.min(posts.length - 1, answers.length - 1)]();
    };
    const client = createApiClient({ origin: FAKE_ORIGIN, auth: { kind: 'session' }, fetch });
    const stream = fakeStreamTransport();
    const sleeps: number[] = [];
    const onSettle = jest.fn(async () => {});
    const controller = TurnController.regeneration(target, {
      client,
      transport: stream.transport,
      appState: null,
      sleep: async (ms) => {
        sleeps.push(ms);
      },
      onSettle,
    });
    expect(controller.getSnapshot()).toMatchObject({
      continuation: true,
      regeneration: { answerUuid: CAPTURE.answerUuid, regenerationId: 'f59416e1-c2ef-436a-8108-674c616d3dcf' },
    });
    const done = controller.start();
    await flush();
    expect(posts).toHaveLength(2);
    expect(posts[0].url).toBe(`${FAKE_ORIGIN}/api/v2/elitea_core/regenerate/prompt_lib/${P}/${CAPTURE.answerUuid}?execution_contract=${REGENERATE_CONTRACT}`);
    expect(posts[1].body).toBe(posts[0].body);
    expect(sleeps).toEqual([1000]);
    expect(stream.connections[0].url).toBe(`${FAKE_ORIGIN}${regenerateStarted.events_url}`);
    stream.connections[0].push(loadFixtureText('regenerate-answer'));
    await done;
    expect(controller.getSnapshot()).toMatchObject({
      settled: true,
      turn: { phase: 'done', content: answerGroupAfterRegenerate.content },
      execution: { responseMessageId: CAPTURE.answerUuid },
    });
    expect(onSettle).toHaveBeenCalledTimes(1);
  });
});

describe('answerHiddenByRegeneration', () => {
  const live = (phase: string, failure: string | null, content = '', settled = false) =>
    ({ settled, failure, turn: { phase, content }, regeneration: { answerUuid: 'A', regenerationId: 'R' } }) as unknown as LiveTurn;

  it('hides the old answer while the new one is sent and streams, and after the stream wrote something', () => {
    expect(answerHiddenByRegeneration(live('sending', null))).toBe('A');
    expect(answerHiddenByRegeneration(live('streaming', null))).toBe('A');
    expect(answerHiddenByRegeneration(live('failed', 'stream'))).toBe('A');
    expect(answerHiddenByRegeneration(live('failed', 'send', 'partial'))).toBe('A');
  });

  it('shows it again when the send failed with nothing written (the server never touched it), or once settled', () => {
    expect(answerHiddenByRegeneration(live('failed', 'send'))).toBeNull();
    expect(answerHiddenByRegeneration(live('done', null, 'new', true))).toBeNull();
    expect(answerHiddenByRegeneration(null)).toBeNull();
  });
});
