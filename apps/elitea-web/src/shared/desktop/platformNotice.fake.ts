/** An in-memory `PlatformNotice` for tests: what was shown, and the last unread count. */
import type { PlatformNotice } from './platformNotice';

export interface FakePlatformNotice extends PlatformNotice {
  readonly shown: { readonly title: string; readonly body: string }[];
  unread: number | undefined;
}

export function createFakePlatformNotice(): FakePlatformNotice {
  const fake: FakePlatformNotice = {
    shown: [],
    unread: undefined,
    show(title, body) {
      fake.shown.push({ title, body });
      return Promise.resolve();
    },
    setUnread(unread) {
      fake.unread = unread;
      return Promise.resolve();
    },
  };
  return fake;
}
