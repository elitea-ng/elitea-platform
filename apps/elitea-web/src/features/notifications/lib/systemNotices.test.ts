import { afterEach, describe, expect, it } from 'vitest';

import { clearNamespace } from '@/shared/lib/storage';

import type { NormalizedNotification } from '../api/normalize';
import { setSystemNoticesEnabled, systemNoticesEnabled, systemNoticeText } from './systemNotices';

afterEach(() => clearNamespace());

describe('system notices setting', () => {
  it('is on until turned off, and remembers the choice', () => {
    expect(systemNoticesEnabled()).toBe(true);
    setSystemNoticesEnabled(false);
    expect(systemNoticesEnabled()).toBe(false);
    setSystemNoticesEnabled(true);
    expect(systemNoticesEnabled()).toBe(true);
  });
});

describe('systemNoticeText', () => {
  it('titles the notice with the kind and shows the message, links as their text', () => {
    const notification = { id: '1', eventType: 'workspace_reclaim_requested', meta: { message: 'Open [the thread](/chat/7) to take it back' } } as unknown as NormalizedNotification;
    const { title, body } = systemNoticeText(notification);
    expect(title).toBe('Workspace reclaim requested');
    expect(body).not.toContain('](');
    expect(body).toContain('to take it back');
  });
});
