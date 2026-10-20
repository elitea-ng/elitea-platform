import { goldenData, nodeOf } from '@/features/chat/__fixtures__/clientFrames';
import {
  ContinueBodyError,
  askUserQuestions,
  buildAnswerValue,
  buildAuthSkipBody,
  buildHitlContinueBody,
  buildOutputLimitBody,
  missingAnswers,
  pauseFromGroup,
  type PauseTarget,
} from '@/features/chat/pauses/continueBodies';
import { pauseOf, type HitlInterrupt, type TurnPause } from '@/features/chat/toolActivity';

const CONV = '0b6c1f8e-1d2a-4f3b-8c4d-5e6f7a8b9c0d';
const target: PauseTarget = { projectId: 7, conversationUuid: CONV, messageId: 'resp-1', threadId: 'thread-1' };

function hitl(pause: TurnPause | null): Extract<TurnPause, { kind: 'hitl' }> {
  if (pause?.kind !== 'hitl') throw new Error('expected a hitl pause');
  return pause;
}

const pythonPause = () => hitl(pauseOf(nodeOf(goldenData('python', 'hitl_interrupt'))));
const rustPause = () => hitl(pauseOf(nodeOf(goldenData('rust', 'hitl_interrupt'))));

const reason = (fn: () => unknown) => {
  try {
    fn();
  } catch (e) {
    return e instanceof ContinueBodyError ? e.reason : 'other';
  }
  return null;
};

describe('buildHitlContinueBody (the web client bodies)', () => {
  it('a tool-call pause answers with one hitl_decisions entry (thread_id at the top)', () => {
    const [interrupt] = pythonPause().interrupts;
    expect(buildHitlContinueBody(target, [{ interrupt, action: 'approve' }])).toEqual({
      project_id: 7,
      conversation_uuid: CONV,
      message_id: 'resp-1',
      thread_id: 'thread-1',
      hitl_resume: true,
      hitl_decisions: [{ interrupt_id: 'interrupt-1', tool_call_id: 'call-1', action: 'approve', value: '' }],
    });
  });

  it('edit sends the edited arguments as JSON TEXT (a string), not an object', () => {
    const [interrupt] = pythonPause().interrupts;
    const body = buildHitlContinueBody(target, [{ interrupt, action: 'edit', value: '{"branch":"main"}' }]);
    expect(body.hitl_decisions).toEqual([{ interrupt_id: 'interrupt-1', tool_call_id: 'call-1', action: 'edit', value: '{"branch":"main"}' }]);
    expect(reason(() => buildHitlContinueBody(target, [{ interrupt, action: 'edit', value: { branch: 'main' } }]))).toBe('value');
  });

  it('a root (pipeline node) pause answers with hitl_action and hitl_value', () => {
    const [interrupt] = rustPause().interrupts;
    expect(buildHitlContinueBody(target, [{ interrupt, action: 'approve' }])).toEqual({
      project_id: 7,
      conversation_uuid: CONV,
      message_id: 'resp-1',
      thread_id: 'thread-1',
      hitl_resume: true,
      hitl_action: 'approve',
    });
    expect(buildHitlContinueBody(target, [{ interrupt, action: 'edit', value: 'A better answer.' }])).toMatchObject({
      hitl_action: 'edit',
      hitl_value: 'A better answer.',
    });
  });

  it('block_with_comment needs its comment', () => {
    const interrupt = { ...pythonPause().interrupts[0], availableActions: ['approve', 'block_with_comment'] };
    expect(reason(() => buildHitlContinueBody(target, [{ interrupt, action: 'block_with_comment', value: '  ' }]))).toBe('value');
    expect(buildHitlContinueBody(target, [{ interrupt, action: 'block_with_comment', value: 'Not this branch.' }]).hitl_decisions).toEqual([
      { interrupt_id: 'interrupt-1', tool_call_id: 'call-1', action: 'block_with_comment', value: 'Not this branch.' },
    ]);
  });

  it('refuses an action the pause did not offer', () => {
    const [interrupt] = pythonPause().interrupts;
    expect(reason(() => buildHitlContinueBody(target, [{ interrupt, action: 'block_with_comment', value: 'x' }]))).toBe('action');
    expect(reason(() => buildHitlContinueBody(target, []))).toBe('action');
  });

  it('a parallel pause answers every interrupt in one body', () => {
    const [first] = pythonPause().interrupts;
    const second: HitlInterrupt = { ...first, interruptId: 'interrupt-2', toolCallId: 'call-2' };
    expect(
      buildHitlContinueBody(target, [
        { interrupt: first, action: 'approve' },
        { interrupt: second, action: 'reject' },
      ]).hitl_decisions,
    ).toEqual([
      { interrupt_id: 'interrupt-1', tool_call_id: 'call-1', action: 'approve', value: '' },
      { interrupt_id: 'interrupt-2', tool_call_id: 'call-2', action: 'reject', value: '' },
    ]);
  });

  it('a fan-out child without a tool call still answers by decision (and needs its interrupt_id)', () => {
    const [root] = rustPause().interrupts;
    const child: HitlInterrupt = { ...root, childThreadId: 'child-1' };
    expect(buildHitlContinueBody(target, [{ interrupt: child, action: 'approve' }]).hitl_decisions).toEqual([
      { interrupt_id: root.interruptId, tool_call_id: '', action: 'approve', value: '' },
    ]);
    expect(reason(() => buildHitlContinueBody(target, [{ interrupt: { ...child, interruptId: null }, action: 'approve' }]))).toBe('interrupt');
  });

  it('refuses a target without a conversation or message id', () => {
    const [interrupt] = pythonPause().interrupts;
    expect(reason(() => buildHitlContinueBody({ ...target, messageId: '' }, [{ interrupt, action: 'approve' }]))).toBe('target');
  });
});

describe('ask_user', () => {
  // The stored shape the web normaliser test pins (ELITEA-2792 fields included).
  const askUser: HitlInterrupt = {
    ...pythonPause().interrupts[0],
    interruptId: 'int-ask-1',
    toolCallId: 'call_mock_ask_user_1',
    guardrailType: 'clarifying_question',
    availableActions: ['answer'],
    questions: [
      { id: 'environment', question: 'Which environment?', options: [{ label: 'Staging' }, { label: 'Prod' }] },
      { id: 'regions', question: 'Which regions?', multiSelect: true, allow_other: true, options: [{ label: 'EU' }, { label: 'US' }] },
      { question: 'Anything else?', optional: true },
    ],
  };

  it('reads the questions, keying one without an id q{n}', () => {
    expect(askUserQuestions(askUser).map((q) => [q.key, q.multiSelect, q.allowOther, q.optional])).toEqual([
      ['environment', false, false, false],
      ['regions', true, true, false],
      ['q3', false, true, true],
    ]);
  });

  it('answers with an object keyed by question, leaving unanswered optional ones out', () => {
    const value = buildAnswerValue(askUser, { environment: ['Staging'], regions: ['EU'] }, { regions: 'APAC' });
    expect(value).toEqual({ environment: 'Staging', regions: ['EU', 'APAC'] });
    expect(missingAnswers(askUser, { environment: ['Staging'] }, {})).toEqual(['regions']);
    expect(buildHitlContinueBody(target, [{ interrupt: askUser, action: 'answer', value }]).hitl_decisions).toEqual([
      { interrupt_id: 'int-ask-1', tool_call_id: 'call_mock_ask_user_1', action: 'answer', value: { environment: 'Staging', regions: ['EU', 'APAC'] } },
    ]);
  });

  it('a pause without questions answers with the bare text', () => {
    const bare: HitlInterrupt = { ...askUser, questions: [] };
    expect(askUserQuestions(bare)).toHaveLength(1);
    expect(buildAnswerValue(bare, {}, { q1: ' staging ' })).toBe('staging');
  });
});

describe('authorization and output limit', () => {
  const auth = pauseOf(nodeOf(goldenData('python', 'mcp_authorization_terminal')));

  it('Skip of one sign-in names the request (python golden frame)', () => {
    if (auth?.kind !== 'auth') throw new Error('expected an auth pause');
    expect(buildAuthSkipBody(target, auth.requests)).toEqual({
      project_id: 7,
      conversation_uuid: CONV,
      message_id: 'resp-1',
      thread_id: 'thread-1',
      hitl_resume: false,
      hitl_decisions: [],
      authorization_request_id: 'auth-1',
      authorization_action: 'skip',
    });
  });

  it('Skip of several goes as mcp_auth decisions, and never carries tokens', () => {
    if (auth?.kind !== 'auth') throw new Error('expected an auth pause');
    const body = buildAuthSkipBody(target, [auth.requests[0], { ...auth.requests[0], requestId: 'auth-2', toolCallId: 'call-2' }]);
    expect(body).toMatchObject({
      hitl_resume: true,
      authorization_request_id: '',
      authorization_action: '',
      hitl_decisions: [
        { interrupt_id: 'auth-1', guardrail_type: 'mcp_auth', action: 'skip' },
        { interrupt_id: 'auth-2', tool_call_id: 'call-2', guardrail_type: 'mcp_auth', action: 'skip' },
      ],
    });
    expect(body).not.toHaveProperty('mcp_tokens');
  });

  it('Continue after the output limit names the paused answer', () => {
    expect(buildOutputLimitBody(target)).toEqual({ project_id: 7, conversation_uuid: CONV, message_id: 'resp-1', thread_id: 'thread-1' });
  });
});

describe('pauseFromGroup (after a reload)', () => {
  const group = (meta: Record<string, unknown>, extra: Record<string, unknown> = {}) => ({ id: 12, uuid: 'resp-1', meta, ...extra });

  it('rebuilds an ask_user pause from hitl_interrupts', () => {
    const pause = hitl(
      pauseFromGroup(
        group({
          thread_id: 'thread-1',
          hitl_interrupts: [{ guardrail_type: 'clarifying_question', available_actions: ['answer'], tool_call_id: 'call_mock_ask_user_1', interrupt_id: 'int-ask-1', questions: [{ id: 'environment', question: 'Which environment?', options: [{ label: 'Staging' }] }] }],
        }),
      ),
    );
    expect(pause).toMatchObject({ shape: 'single', threadId: 'thread-1', messageId: 'resp-1' });
    expect(pause.interrupts[0]).toMatchObject({ interruptId: 'int-ask-1', availableActions: ['answer'] });
  });

  it('rebuilds every sign-in request', () => {
    const authorization = (id: string, call: string) => ({
      interrupt_id: id,
      tool_call_id: call,
      guardrail_type: 'mcp_auth',
      tool_name: 'SharePoint search',
      toolkit_type: 'sharepoint',
      server_url: 'https://sharepoint.example.test',
      resource_metadata: { resource_name: 'SharePoint' },
    });
    const pause = pauseFromGroup(group({ authorization_requests: [authorization('auth-1', 'call-1'), authorization('auth-2', 'call-2')] }));
    expect(pause?.kind === 'auth' && pause.requests.map((r) => r.requestId)).toEqual(['auth-1', 'auth-2']);
  });

  it('rebuilds the output-limit offer, reading `metadata` too', () => {
    expect(pauseFromGroup({ uid: 'resp-1', metadata: { output_limit_reached: true, thread_id: 'thread-1' } })).toEqual({
      kind: 'output_limit',
      threadId: 'thread-1',
      messageId: 'resp-1',
      action: 'Continue',
    });
  });

  it('is nothing for a streaming answer, an answer without pause metadata, or an empty list', () => {
    expect(pauseFromGroup(group({ output_limit_reached: true }, { is_streaming: true }))).toBeNull();
    expect(pauseFromGroup(group({ thread_id: 't' }))).toBeNull();
    expect(pauseFromGroup(group({ hitl_interrupts: [] }))).toBeNull();
    expect(pauseFromGroup(null)).toBeNull();
  });
});
