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

  it('cuts by code points, never through an emoji, and counts as the host does', async () => {
    const invoke = vi.fn().mockResolvedValue(undefined);
    // 119 letters then a run of astral emoji: a UTF-16 cut at 119 lands inside the first one.
    const body = `${'a'.repeat(NOTICE_BODY_MAX - 2)}😀😀😀😀`;
    await createPlatformNotice(invoke).show('Run 🚀 done', body);
    const [, args] = invoke.mock.calls[0] as [string, { title: string; body: string }];
    expect(args.title).toBe('Run 🚀 done');
    expect(Array.from(args.body)).toHaveLength(NOTICE_BODY_MAX);
    expect(args.body).toBe(`${'a'.repeat(NOTICE_BODY_MAX - 2)}😀…`);
    // No lone surrogate: every high surrogate is followed by a low one.
    expect(/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/.test(args.body)).toBe(false);
  });

  it('keeps a body of exactly the limit in emoji whole', async () => {
    const invoke = vi.fn().mockResolvedValue(undefined);
    const body = '😀'.repeat(NOTICE_BODY_MAX);
    await createPlatformNotice(invoke).show('t', body);
    const [, args] = invoke.mock.calls[0] as [string, { title: string; body: string }];
    expect(args.body).toBe(body);
  });

  it('replaces a lone surrogate, which the host could not decode', async () => {
    const invoke = vi.fn().mockResolvedValue(undefined);
    await createPlatformNotice(invoke).show('t', 'broken \uD83D here');
    const [, args] = invoke.mock.calls[0] as [string, { title: string; body: string }];
    expect(args.body).toBe('broken \uFFFD here');
  });
});
