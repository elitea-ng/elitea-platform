import { describe, expect, it } from 'vitest';

import { OFF_STATUS, type IndexEvent, type IndexPhase, type IndexState } from '@/shared/desktop/indexIpc';

import { foldProgress } from './progress';

const event = (phase: IndexPhase, message: string | null, state: IndexState = 'building'): IndexEvent => ({
  workspace_id: 'w1',
  phase,
  message,
  status: { ...OFF_STATUS, state },
});

describe('foldProgress', () => {
  it('reads the file total, then the files processed, from the engine lines', () => {
    let progress = foldProgress(null, event('listing', null));
    expect(progress).toEqual({ done: 0, total: null });
    progress = foldProgress(progress, event('parsing', '[extract] Parsing 340 files'));
    expect(progress).toEqual({ done: 0, total: 340 });
    progress = foldProgress(progress, event('building', '[progress] 📄 Processed 120 files | 📊 900 entities'));
    expect(progress).toEqual({ done: 120, total: 340 });
    progress = foldProgress(progress, event('building', '[relations] 🔗 Adding 12 parser-extracted relationships...'));
    expect(progress).toEqual({ done: 120, total: 340 });
    expect(foldProgress(progress, event('saving', null))).toEqual({ done: 340, total: 340 });
  });

  it('ends with the build', () => {
    expect(foldProgress({ done: 3, total: 9 }, event('ready', null, 'ready'))).toBeNull();
    expect(foldProgress({ done: 3, total: 9 }, event('cancelled', null, 'stale'))).toBeNull();
    expect(foldProgress({ done: 3, total: 9 }, event('error', 'boom', 'error'))).toBeNull();
  });
});
