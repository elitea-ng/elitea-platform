import { describe, expect, it } from 'vitest';

import type { LspEvent } from '@/shared/desktop/lspIpc';

import { applyLspEvent } from './LspChip';

const event = (state: LspEvent['payload']['state'], workspace = 'w1', message?: string): LspEvent => ({
  workspace_id: workspace,
  seq: 0,
  kind: 'lsp_status',
  payload: message === undefined ? { language: 'rust', state } : { language: 'rust', state, message },
});

describe('applyLspEvent', () => {
  it('keeps the latest state per language and drops a stopped server', () => {
    let shown = applyLspEvent({}, 'w1', event('starting'));
    shown = applyLspEvent(shown, 'w1', event('ready'));
    expect(shown).toEqual({ rust: { state: 'ready', message: undefined } });
    expect(applyLspEvent(shown, 'w1', event('stopped'))).toEqual({});
  });

  it('ignores another folder and keeps why a server is off', () => {
    expect(applyLspEvent({}, 'w1', event('ready', 'w2'))).toEqual({});
    const off = applyLspEvent({}, 'w1', event('unavailable', 'w1', 'rust-analyzer not found on PATH'));
    expect(off.rust?.message).toBe('rust-analyzer not found on PATH');
  });
});
