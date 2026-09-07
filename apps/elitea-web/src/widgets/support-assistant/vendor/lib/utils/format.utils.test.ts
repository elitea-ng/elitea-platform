/**
 * `formatTime` — the timestamp shown beside every message bubble.
 *
 * Three branches: today (bare time), yesterday (prefixed), older (dated).
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { formatTime } from './format.utils';

describe('formatTime', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-06-15T15:30:00'));
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('shows a bare time for a timestamp from TODAY', () => {
    const today = new Date('2026-06-15T09:05:00').getTime();
    expect(formatTime(today)).not.toContain('Yesterday');
    expect(formatTime(today)).toMatch(/\d{1,2}:\d{2}/);
  });

  it('prefixes "Yesterday" for a timestamp from the day before', () => {
    const yesterday = new Date('2026-06-14T09:05:00').getTime();
    expect(formatTime(yesterday)).toMatch(/^Yesterday, /);
  });

  it('shows a full date for anything OLDER than yesterday', () => {
    const older = new Date('2026-06-01T09:05:00').getTime();
    const result = formatTime(older);
    expect(result).not.toMatch(/^Yesterday/);
    expect(result).toContain('Jun');
  });
});
