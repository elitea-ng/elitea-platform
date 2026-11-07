/**
 * A fake view stream for tests: a `BrowserViewSocket` the test drives as
 * the server would (hello, frame header + JPEG, notices, end, close codes),
 * recording what the client sent. Used by the client's and the pane's tests.
 */
import type { BrowserViewSocket, BrowserViewSocketFactory } from './client';

export class FakeBrowserViewSocket implements BrowserViewSocket {
  binaryType: BinaryType = 'blob';
  readyState = 0;
  readonly sent: Record<string, unknown>[] = [];
  closedWith: number | undefined;
  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onclose: ((event: CloseEvent) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;

  constructor(readonly url: string) {}

  send(data: string): void {
    this.sent.push(JSON.parse(data) as Record<string, unknown>);
  }

  close(code?: number): void {
    this.closedWith = code;
    this.readyState = 3;
  }

  /** The server: the socket opened and said `hello`. */
  hello(canControl: boolean, lingering = false, sessionId = 'elitea-br-000000000000000000000000'): void {
    this.readyState = 1;
    this.onopen?.(new Event('open'));
    this.text({ type: 'hello', session_id: sessionId, can_control: canControl, lingering });
  }

  text(message: unknown): void {
    this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(message) }));
  }

  /** One frame: its header and its JPEG. */
  frame(seq: number, extra: { url?: string; title?: string; control?: 'agent' | 'user'; width?: number; height?: number } = {}): void {
    const jpeg = new Blob([new Uint8Array([0xff, 0xd8, 0xff, 0xd9])], { type: 'image/jpeg' });
    this.text({ type: 'frame', seq, width: extra.width ?? 1280, height: extra.height ?? 800, bytes: jpeg.size, url: extra.url ?? 'https://example.test/', title: extra.title ?? 'Example', control: extra.control ?? 'agent' });
    this.onmessage?.(new MessageEvent('message', { data: jpeg }));
  }

  serverClose(code: number, reason = ''): void {
    this.readyState = 3;
    this.onclose?.(new CloseEvent('close', { code, reason }));
  }
}

/** A factory that keeps every socket it made; `last()` is the newest. */
export function fakeSocketFactory(): { factory: BrowserViewSocketFactory; sockets: FakeBrowserViewSocket[]; last: () => FakeBrowserViewSocket } {
  const sockets: FakeBrowserViewSocket[] = [];
  const factory: BrowserViewSocketFactory = (url) => {
    const socket = new FakeBrowserViewSocket(url);
    sockets.push(socket);
    return socket;
  };
  return {
    factory,
    sockets,
    last: () => {
      const socket = sockets[sockets.length - 1];
      if (socket === undefined) throw new Error('no socket was opened');
      return socket;
    },
  };
}
