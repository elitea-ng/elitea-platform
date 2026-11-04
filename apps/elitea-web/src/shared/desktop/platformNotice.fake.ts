/** An in-memory `PlatformNotice` for tests: what was shown, and the last unread count. */
import type { PlatformNotice, PlatformNoticeAbout } from './platformNotice';

export interface FakePlatformNotice extends PlatformNotice {
  readonly shown: { readonly title: string; readonly body: string; readonly about?: PlatformNoticeAbout | undefined }[];
  unread: number | undefined;
}

export function createFakePlatformNotice(): FakePlatformNotice {
  const fake: FakePlatformNotice = {
    shown: [],
    unread: undefined,
    show(title, body, about) {
      fake.shown.push({ title, body, about });
      return Promise.resolve();
    },
    setUnread(unread) {
      fake.unread = unread;
      return Promise.resolve();
    },
  };
  return fake;
}
