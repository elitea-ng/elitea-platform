/**
 * The socket the cloud browser view stream runs on (`./client`). Its own
 * module with no imports, so `shared/api/nativeTransport` can name the type a
 * desktop host supplies without importing the client (which imports it).
 */
/** The part of `WebSocket` the client touches; a desktop host socket implements the same. */
export interface BrowserViewSocket {
  binaryType: BinaryType;
  readonly readyState: number;
  send(data: string): void;
  close(code?: number, reason?: string): void;
  onopen: ((event: Event) => void) | null;
  onmessage: ((event: MessageEvent) => void) | null;
  onclose: ((event: CloseEvent) => void) | null;
  onerror: ((event: Event) => void) | null;
}

export type BrowserViewSocketFactory = (url: string) => BrowserViewSocket;
