import { describe, expect, it } from 'vitest';

import {
  addIndexRunProgress,
  buildIndexRunSummaryMessage,
  ensureRunMessage,
  settleIndexRunProgress,
} from './indexRunNotices.helpers';

const NOW = 1_700_000_060_000;

describe('addIndexRunProgress / settleIndexRunProgress', () => {
  it('shows a running placeholder once, and never stacks a second one', () => {
    const once = addIndexRunProgress([], NOW);
    expect(once).toHaveLength(1);
    expect(once[0]).toMatchObject({ id: 'index-run-progress', isLoading: true });
    expect(addIndexRunProgress(once, NOW)).toHaveLength(1);
  });

  it('replaces exactly the placeholder with the summary and keeps every other message', () => {
    const keep = { id: 'keep', role: 'assistant', content: 'x', created_at: 1, participant_id: 'p' };
    const history = addIndexRunProgress([keep], NOW);
    const summary = buildIndexRunSummaryMessage({ status: 'ok', message: 'done' }, NOW - 5_000, NOW);
    const settled = settleIndexRunProgress(history, summary);
    expect(settled.map((message) => message.id)).toEqual(['keep', summary.id]);
  });
});

describe('buildIndexRunSummaryMessage', () => {
  it('states the outcome, the worker sentence, the typed counts and the duration', () => {
    const message = buildIndexRunSummaryMessage(
      {
        status: 'partly_indexed',
        message: 'Indexed 4 documents.',
        indexed_documents: 4,
        indexed_chunks: 19,
        failed_chunks: 1,
        unchanged_documents: 2,
        total_documents: 23,
        skipped: { too_large: 2, unsupported_type: 1 },
      },
      NOW - 75_000,
      NOW,
    );
    expect(message.isStreaming).toBe(false);
    expect(message.content).toContain('⚠️');
    expect(message.content).toContain('(1 min 15 s)');
    expect(message.content).toContain('Indexed 4 documents.');
    expect(message.content).toContain('- 4 documents indexed (19 chunks)');
    expect(message.content).toContain('- 2 documents already indexed (unchanged)');
    expect(message.content).toContain('- Skipped: too large 2, unsupported type 1');
    expect(message.content).toContain('- 1 chunk could not be embedded or written');
    expect(message.content).toContain('- In the index now: 23 documents');
  });

  it('reads a failed run as a failure, with the reason', () => {
    const message = buildIndexRunSummaryMessage({ status: 'error', message: "Index 'x' failed: no model" }, NOW - 3_000, NOW);
    expect(message.content).toContain('❌');
    expect(message.content).toContain("Index 'x' failed: no model");
    expect(message.content).toContain('(3 s)');
  });

  it('says only what an older worker frame says: no counts invented, no duration without a start', () => {
    const message = buildIndexRunSummaryMessage({ status: 'ok', message: 'No new documents to index.' }, undefined, NOW);
    expect(message.content).toBe('✅ Indexing finished\n\nNo new documents to index.');
  });

  it('drops a skip reason that is not a closed token and any non-numeric count', () => {
    const message = buildIndexRunSummaryMessage(
      { status: 'ok', indexed_documents: 'many', skipped: { 'Bad Reason!': 3, ok_reason: -1, fine: 2 } },
      undefined,
      NOW,
    );
    expect(message.content).toContain('Skipped: fine 2');
    expect(message.content).not.toContain('Bad');
    expect(message.content).not.toContain('many');
  });
});

describe('ensureRunMessage', () => {
  it('creates the run message for a step frame that names one this panel has not seen, replacing the placeholder', () => {
    const history = addIndexRunProgress([], NOW);
    const next = ensureRunMessage(history, 'agent_thinking_step', 'run-1', NOW);
    expect(next.map((message) => message.id)).toEqual(['run-1']);
    expect(next[0]).toMatchObject({ isLoading: true, isStreaming: true });
  });

  it('leaves history alone for a known message, an unrelated frame type or a frame with no message id', () => {
    const known = ensureRunMessage([], 'agent_tool_start', 'run-1', NOW);
    expect(ensureRunMessage(known, 'agent_tool_end', 'run-1', NOW)).toHaveLength(1);
    expect(ensureRunMessage([], 'start_task', 'run-1', NOW)).toHaveLength(0);
    // `start_task` brings its own running message: only the placeholder goes.
    expect(ensureRunMessage(addIndexRunProgress([], NOW), 'start_task', 'run-1', NOW)).toHaveLength(0);
    expect(ensureRunMessage([], 'agent_tool_start', undefined, NOW)).toHaveLength(0);
    expect(ensureRunMessage([], 'agent_tool_start', '', NOW)).toHaveLength(0);
  });
});
