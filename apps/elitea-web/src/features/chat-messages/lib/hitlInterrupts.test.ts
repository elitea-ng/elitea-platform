import { describe, expect, it } from 'vitest';

import { getInterruptIdentity, normalizeHitlInterrupt, reconcileHitlInterrupts } from './hitlInterrupts';

import type { HitlInterrupt } from '../ui/chat-hitl-actions/ChatHitlActions';

describe('normalizeHitlInterrupt', () => {
  it('takes the parent and child thread from the overlay the child cannot know', () => {
    // A fan-out child does not know it is a child: the indexer stamps the
    // parent name and the child thread into event metadata, and the overlay is
    // the only place they exist.
    const interrupt = normalizeHitlInterrupt(
      { tool_name: 'delete_file', tool_call_id: 'call-1' },
      { parent_agent_name: 'planner', child_thread_id: 'thread-child', thread_id: 'thread-child' },
    );

    expect(interrupt.parent_agent_name).toBe('planner');
    expect(interrupt.child_thread_id).toBe('thread-child');
    expect(interrupt.thread_id).toBe('thread-child');
  });

  it('marks a child pause aggregate_child and a plain pause single', () => {
    // The strategy is what routes resume: a child resumes on its own thread,
    // a single pause on the message's.
    expect(normalizeHitlInterrupt({}, { child_thread_id: 't' }).resume_strategy).toBe('aggregate_child');
    expect(normalizeHitlInterrupt({}).resume_strategy).toBe('single');
    // An explicit strategy still wins over both.
    expect(normalizeHitlInterrupt({ resume_strategy: 'custom' }, { child_thread_id: 't' }).resume_strategy).toBe(
      'custom',
    );
  });

  it('always yields answerable actions and a message', () => {
    // An interrupt with no actions renders a card the user cannot answer, which
    // stalls the run with no way out.
    const interrupt = normalizeHitlInterrupt({});
    expect(interrupt.available_actions).toEqual(['approve', 'reject']);
    expect(interrupt.message).toBe('Please review and take action.');
  });

  it('keeps a falsy tool_args instead of replacing it with null', () => {
    // `??`, not `||`: an empty-object or empty-string argument set is a real
    // value the approval card must show.
    expect(normalizeHitlInterrupt({ tool_args: '' }).tool_args).toBe('');
    expect(normalizeHitlInterrupt({ tool_args: 0 }).tool_args).toBe(0);
    expect(normalizeHitlInterrupt({}).tool_args).toBeNull();
  });

  it('carries an ask_user pause’s questions through, and defaults them to []', () => {
    // The `answer` card has no other source for them: a normaliser that drops
    // `questions` renders the pause's question text with no controls under it,
    // and the run can never be resumed. Measured live before this line existed.
    const questions = [{ id: 'environment', question: 'Which environment?', options: [{ label: 'Staging' }] }];
    expect(normalizeHitlInterrupt({ guardrail_type: 'clarifying_question', questions }).questions).toEqual(questions);
    // From the OVERLAY too — the same `pick` both halves of a pause are read with.
    expect(normalizeHitlInterrupt({}, { questions }).questions).toEqual(questions);
    // Never undefined, and never a non-array: the card maps over this.
    expect(normalizeHitlInterrupt({}).questions).toEqual([]);
    expect(normalizeHitlInterrupt({ questions: 'not-an-array' }).questions).toEqual([]);
  });

  it('produces a value the approval card accepts', () => {
    // Compile-time pin: the reducer's output type and the renderer's prop type
    // are declared separately, and a drift between them would only surface at
    // the call site that wires the two together.
    const interrupt: HitlInterrupt = normalizeHitlInterrupt({ tool_name: 'x' });
    expect(interrupt.tool_name).toBe('x');
  });
});

describe('getInterruptIdentity', () => {
  it('prefers the backend id, then the thread/tool pair', () => {
    expect(getInterruptIdentity({ interrupt_id: 'i-1', tool_call_id: 'c-1' })).toBe('i-1');
    expect(getInterruptIdentity({ thread_id: 't', tool_call_id: 'c' })).toBe(getInterruptIdentity({ thread_id: 't', tool_call_id: 'c' }));
  });

  it('distinguishes two approvals from ONE aggregate child', () => {
    // Same thread, different tool calls. Collapsing them would drop an approval
    // the user still owes an answer to.
    const first = getInterruptIdentity({ thread_id: 't', tool_call_id: 'c-1' });
    const second = getInterruptIdentity({ thread_id: 't', tool_call_id: 'c-2' });
    expect(first).not.toBe(second);
  });

  it('identifies nothing when nothing identifies it', () => {
    expect(getInterruptIdentity({ message: 'approve?' })).toBe('');
    expect(getInterruptIdentity(undefined)).toBe('');
    expect(getInterruptIdentity({ thread_id: '   ' })).toBe('');
  });
});

describe('reconcileHitlInterrupts', () => {
  const entry = (raw: Record<string, unknown>) => normalizeHitlInterrupt(raw);

  it('replaces a re-announced pause instead of duplicating it', () => {
    const merged = reconcileHitlInterrupts(
      [entry({ interrupt_id: 'i-1', message: 'first' })],
      [entry({ interrupt_id: 'i-1', message: 'revised' })],
    );

    expect(merged).toHaveLength(1);
    expect(merged[0]?.message).toBe('revised');
  });

  it('keeps siblings that are still awaiting an answer', () => {
    // Cards announce one frame at a time; replacing the array wholesale would
    // drop the earlier sibling's card while it is still pending.
    const merged = reconcileHitlInterrupts([entry({ interrupt_id: 'i-1' })], [entry({ interrupt_id: 'i-2' })]);

    expect(merged.map((item) => item.interrupt_id)).toEqual(['i-1', 'i-2']);
  });

  it('appends unidentifiable entries rather than letting them overwrite each other', () => {
    const merged = reconcileHitlInterrupts([entry({ message: 'a' })], [entry({ message: 'b' })]);
    expect(merged.map((item) => item.message)).toEqual(['a', 'b']);
  });

  it('keeps the local queued/decided state of a re-announced card, hidden', () => {
    const merged = reconcileHitlInterrupts(
      [{ ...entry({ interrupt_id: 'i-1' }), queued: true, hidden: true }],
      [entry({ interrupt_id: 'i-1', message: 'again' })],
    );
    expect(merged[0]).toMatchObject({ interrupt_id: 'i-1', message: 'again', queued: true, decided: false, hidden: true });
  });

  it('drops a submitted card the new pause does not name', () => {
    const merged = reconcileHitlInterrupts(
      [{ ...entry({ interrupt_id: 'old' }), decided: true, hidden: true }],
      [entry({ interrupt_id: 'new' })],
    );
    expect(merged.map((item) => item.interrupt_id)).toEqual(['new']);
  });

  it('drops cards a parallel aggregate does not name, and never adds one', () => {
    const merged = reconcileHitlInterrupts([entry({ interrupt_id: 'i-1' }), entry({ interrupt_id: 'i-2' })], [], ['i-2', 'i-3']);
    expect(merged.map((item) => item.interrupt_id)).toEqual(['i-2']);
  });
});

describe('normalizeHitlInterrupt — hierarchy bounds', () => {
  it('keeps a positive sibling ordinal and drops a path past three tiers', () => {
    const tier = (n: number) => ({ name: `agent-${n}`, call_id: `call-${n}` });
    expect(normalizeHitlInterrupt({ sibling_ordinal: 2 }).sibling_ordinal).toBe(2);
    expect(normalizeHitlInterrupt({ sibling_ordinal: 0 }).sibling_ordinal).toBeUndefined();
    expect(normalizeHitlInterrupt({ parent_agent_path: [tier(1), tier(2), tier(3)] }).parent_agent_path).toHaveLength(3);
    expect(normalizeHitlInterrupt({ parent_agent_path: [tier(1), tier(2), tier(3), tier(4)] }).parent_agent_path).toEqual([]);
  });
});
