/**
 * chatStreamSettle.test.ts — what the user sees when a run ends badly.
 *
 * The case that motivates this file was measured on a live stack: the native
 * Rust runtime refused an agent profile and elitea-main durably recorded
 * `execution.failed` with `{"code":"UNSUPPORTED_CAPABILITY","safe_message":
 * "Configuration type is not supported.","retryable":false}`, and the browser
 * showed nothing at all — no card, no text. `settleInFlight` only mutates
 * messages that are already streaming, and the refusal beat the first frame
 * that would have created one.
 */
import { describe, expect, it } from 'vitest';

import { ROLES } from '@/shared/lib/enums';

import { recordStreamFailure, runtimeFailureReason, settleInFlight } from './chatStreamSettle';
import type { ChatMessage } from './convertMessagesToChatHistory';

const CONTEXT = { name: 'Agent', now: () => '2026-08-29T00:00:00.000Z' };

function question(): ChatMessage {
  return {
    id: 'q-1',
    role: ROLES.User,
    name: 'Alice',
    content: 'why is the sky blue?',
    createdAt: '2026-08-29T00:00:00.000Z',
  };
}

function streamingAnswer(): ChatMessage {
  return {
    id: 'a-1',
    role: ROLES.Assistant,
    name: 'Agent',
    content: 'partial',
    createdAt: '2026-08-29T00:00:00.000Z',
    isStreaming: true,
    isLoading: true,
  };
}

describe('runtimeFailureReason', () => {
  it("quotes the server's own sentence", () => {
    expect(
      runtimeFailureReason({
        code: 'UNSUPPORTED_CAPABILITY',
        safe_message: 'Configuration type is not supported.',
        retryable: false,
      }),
    ).toBe('Configuration type is not supported.');
  });

  it('names the code when the payload carries no sentence', () => {
    // Better than "The agent run failed.": the code is what a support thread
    // can be searched for.
    expect(runtimeFailureReason({ code: 'UNSUPPORTED_CAPABILITY', retryable: false })).toBe(
      'UNSUPPORTED_CAPABILITY',
    );
  });

  it('falls back to the generic sentence for a payload it cannot read', () => {
    expect(runtimeFailureReason({ retryable: true })).toBe('The agent run failed.');
    expect(runtimeFailureReason({ safe_message: '   ' })).toBe('The agent run failed.');
    // `error` is the key the transport used to read. Pinned as NOT read, so a
    // future reader does not re-add it on the assumption that it once worked.
    expect(runtimeFailureReason({ error: 'model unavailable' })).toBe('The agent run failed.');
  });
});

describe('recordStreamFailure', () => {
  it('preserves partial output while explaining incomplete automatic continuation', () => {
    const reason = runtimeFailureReason({
      code: 'OUTPUT_CONTINUATION_EXHAUSTED',
      safe_message: 'Automatic continuation could not finish. The model response is incomplete.',
      retryable: false,
    });
    const next = recordStreamFailure([question(), streamingAnswer()], reason, CONTEXT, undefined, undefined, 'OUTPUT_CONTINUATION_EXHAUSTED');
    expect(next).toHaveLength(2);
    expect(next[1]?.content).toBe('partial');
    expect(next[1]?.failureCode).toBe('OUTPUT_CONTINUATION_EXHAUSTED');
    expect(next[1]?.exception).toBe(reason);
    expect(next[1]?.isStreaming).toBe(false);
    expect(next[1]?.isLoading).toBe(false);
  });

  it('makes an early refusal visible even though no message was ever created', () => {
    // The defect: `settleInFlight` matched nothing and returned the history
    // unchanged, so the composer re-enabled over a transcript that never
    // mentioned the refusal.
    const history = [question()];

    const next = recordStreamFailure(history, 'Configuration type is not supported.', CONTEXT, 'q-1');

    expect(next).toHaveLength(2);
    const failure = next[1];
    expect(failure?.role).toBe(ROLES.Assistant);
    expect(failure?.exception).toBe('Configuration type is not supported.');
    expect(failure?.isStreaming).toBe(false);
    expect(failure?.isLoading).toBe(false);
    // The link back to the question survives, so regenerate can reach it.
    expect(failure?.questionId).toBe('q-1');
  });

  it('keeps the question the turn was refused for', () => {
    const next = recordStreamFailure([question()], 'nope', CONTEXT);

    expect(next[0]).toEqual(question());
  });

  it('settles the streaming message rather than appending a second one', () => {
    const next = recordStreamFailure([question(), streamingAnswer()], 'model unavailable', CONTEXT);

    expect(next).toHaveLength(2);
    expect(next[1]?.exception).toBe('model unavailable');
    expect(next[1]?.content).toBe('partial');
    expect(next[1]?.isStreaming).toBe(false);
  });

  it('appends to an empty transcript too', () => {
    const next = recordStreamFailure([], 'The connection to the agent run was lost.', CONTEXT);

    expect(next).toHaveLength(1);
    expect(next[0]?.exception).toBe('The connection to the agent run was lost.');
  });
});

describe('settleInFlight', () => {
  it('returns the same array when nothing is in flight', () => {
    // `recordStreamFailure` reads this identity to decide whether it has to
    // append, so it is contract rather than an optimisation.
    const history = [question()];

    expect(settleInFlight(history, 'boom')).toBe(history);
  });
});
