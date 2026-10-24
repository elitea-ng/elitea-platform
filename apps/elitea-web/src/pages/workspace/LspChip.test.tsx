import { describe, expect, it } from 'vitest';

import type { LspEvent } from '@/shared/desktop/lspIpc';

import { applyLspEvent, applyLspSnapshot } from './LspChip';

const event = (state: LspEvent['payload']['state'], seq = 0, workspace = 'w1', message?: string): LspEvent => ({
  workspace_id: workspace,
  seq,
  kind: 'lsp_status',
  payload: message === undefined ? { language: 'rust', state } : { language: 'rust', state, message },
});

describe('applyLspEvent', () => {
  it('keeps the latest state per language and marks a stopped server', () => {
    let shown = applyLspEvent({}, 'w1', event('starting', 1));
    shown = applyLspEvent(shown, 'w1', event('ready', 2));
    expect(shown).toEqual({ rust: { state: 'ready', message: undefined, seq: 2 } });
    expect(applyLspEvent(shown, 'w1', event('stopped', 3)).rust?.state).toBe('stopped');
  });

  it('drops an event older than what the language shows', () => {
    const shown = applyLspEvent({}, 'w1', event('ready', 5));
    expect(applyLspEvent(shown, 'w1', event('starting', 4))).toBe(shown);
  });

  it('ignores another folder and keeps why a server is off', () => {
    expect(applyLspEvent({}, 'w1', event('ready', 0, 'w2'))).toEqual({});
    const off = applyLspEvent({}, 'w1', event('unavailable', 0, 'w1', 'rust-analyzer not found on PATH'));
    expect(off.rust?.message).toBe('rust-analyzer not found on PATH');
  });
});

describe('applyLspSnapshot', () => {
  it('seeds a chip mounted after the server started', () => {
    const shown = applyLspSnapshot({}, { seq: 7, statuses: [{ language: 'go', state: 'ready' }] });
    expect(shown.go).toEqual({ state: 'ready', message: undefined, seq: 6 });
    // An event the snapshot already reflects changes nothing it would not.
    expect(applyLspEvent(shown, 'w1', { ...event('ready', 6), payload: { language: 'go', state: 'ready' } }).go?.state).toBe('ready');
  });

  it('keeps what an event since the snapshot says, and drops what it no longer lists', () => {
    let shown = applyLspEvent({}, 'w1', event('crashed', 9));
    shown = applyLspEvent(shown, 'w1', { ...event('ready', 2), payload: { language: 'python', state: 'ready' } });
    const next = applyLspSnapshot(shown, { seq: 8, statuses: [{ language: 'rust', state: 'ready' }] });
    expect(next.rust?.state).toBe('crashed');
    expect(next.python).toBeUndefined();
  });
});
