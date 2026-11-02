import { describe, expect, it, vi } from 'vitest';

import { createPlatformNotice, NOTICE_BODY_MAX } from './platformNotice';

describe('createPlatformNotice', () => {
  it('sends one-line texts, the body cut to the host limit', async () => {
    const invoke = vi.fn().mockResolvedValue(undefined);
    await createPlatformNotice(invoke).show('  Agent\nshared ', 'x'.repeat(NOTICE_BODY_MAX + 10));
    const [command, args] = invoke.mock.calls[0] as [string, { title: string; body: string }];
    expect(command).toBe('platform_notice_show');
    expect(args.title).toBe('Agent shared');
    expect(args.body).toHaveLength(NOTICE_BODY_MAX);
    expect(args.body.endsWith('…')).toBe(true);
  });

  it('sends the unread count as a whole, non-negative number', async () => {
    const invoke = vi.fn().mockResolvedValue(undefined);
    const notice = createPlatformNotice(invoke);
    await notice.setUnread(3.7);
    await notice.setUnread(-2);
    expect(invoke.mock.calls).toEqual([
      ['platform_notice_badge', { unread: 3 }],
      ['platform_notice_badge', { unread: 0 }],
    ]);
  });
});
