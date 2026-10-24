import { describe, expect, it, vi } from 'vitest';

import { LSP_EVENT_CHANNEL, onLspEvent, tauriLspListen, type LspEvent } from './lspIpc';
import type { ListenFn } from './workspaceIpc';

describe('onLspEvent', () => {
  it('listens on lsp://event and hands the payload over', async () => {
    const listen = vi.fn<ListenFn>((_channel, handler) => {
      handler({ workspace_id: 'w1', seq: 0, kind: 'lsp_status', payload: { language: 'rust', state: 'ready' } });
      return Promise.resolve(() => undefined);
    });
    const received: LspEvent[] = [];
    await onLspEvent(listen, (event) => received.push(event));
    expect(listen.mock.calls[0]?.[0]).toBe(LSP_EVENT_CHANNEL);
    expect(received.map((event) => event.payload.state)).toEqual(['ready']);
  });

  it('has no listener outside the desktop webview', () => {
    expect(tauriLspListen()).toBeUndefined();
  });
});
