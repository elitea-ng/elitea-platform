/**
 * CanvasEditor.presence.test.tsx — the COMPOSITION half of #622.
 *
 * `useCanvasPresence` and `useCanvasRoom` are each covered on their own. What
 * is only observable HERE is whether this editor composes them at all, and that
 * is the failure this repository keeps finding: both halves correct, the wiring
 * absent (#597, #126). Before this change `useCanvasRoom` had ZERO consumers —
 * the editor emitted `chat_canvas_edit` into a room it had never joined — and
 * the presence listener sat commented out.
 *
 * These cases fail on the pre-#622 editor: no join is emitted, no heartbeat is
 * sent, no presence row renders, and a burst of keystrokes produces one emit per
 * keystroke instead of one per pause.
 */
import type { ReactElement } from 'react';

import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient, type TestSocketClient } from '@/shared/api/socket/testing';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { CanvasEditor } from './CanvasEditor';

const globals = globalThis as unknown as Record<string, unknown>;
const PRESENCE_PATH = '/api/v2/elitea_core/canvas/prompt_lib/:projectId/:canvasId/presence';
const CANVAS_UUID = 'aaaaaaaa-0000-4000-8000-000000000001';

installCodeMirrorTestPolyfills();

const BLOCK = { codeBlock: 'hello', language: 'markdown', isBlock: true, canvasId: CANVAS_UUID };

let registry: TestEventSourceRegistry;
let client: TestSocketClient;

interface Beat {
  readonly state: string;
  readonly url: string;
}

function installPresenceRoute(editors: readonly { user_id: string; user_name: string }[]): { readonly beats: Beat[] } {
  const beats: Beat[] = [];
  server.use(
    http.post(PRESENCE_PATH, async ({ request }) => {
      const body = (await request.json()) as { state?: string };
      beats.push({ state: body.state ?? 'viewing', url: new URL(request.url).pathname });
      return HttpResponse.json({
        project_id: '7',
        entity_id: CANVAS_UUID,
        entity_type: 'canvas',
        action: 'editors',
        canvas_uuid: CANVAS_UUID,
        message_group_uuid: 'group-1',
        editors,
        ttl_seconds: 120,
      });
    }),
  );
  return { beats };
}

function withSocket(ui: ReactElement): ReactElement {
  return <SocketClientContext.Provider value={client}>{ui}</SocketClientContext.Provider>;
}

function getContent(container: HTMLElement): HTMLElement {
  const content = container.querySelector('.cm-content');
  if (!(content instanceof HTMLElement)) throw new Error('CodeMirror content element not found');
  return content;
}

beforeEach(() => {
  registry = installTestEventSource();
  client = createTestSocketClient();
  globals['elitea_ui_config'] = { vite_server_url: '/api/v2', vite_base_uri: '/', vite_public_project_id: 'public-1' };
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  registry.restore();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
  resetGeneratedClient();
});

describe('CanvasEditor — presence composition (#622)', () => {
  it('joins the canvas socket room it emits edits into', async () => {
    installPresenceRoute([]);
    renderWithTheme(
      withSocket(<CanvasEditor selectedCodeBlockInfo={BLOCK} projectId="7" onCloseCanvasEditor={vi.fn()} />),
    );

    await waitFor(() => {
      expect(client.getEmitted('chat_canvas_join')).toHaveLength(1);
    });
    expect(client.getEmitted('chat_canvas_join')[0]?.payload).toMatchObject({
      canvas_uuid: CANVAS_UUID,
      project_id: '7',
    });
  });

  it('leaves the canvas room on unmount', async () => {
    installPresenceRoute([]);
    const { unmount } = renderWithTheme(
      withSocket(<CanvasEditor selectedCodeBlockInfo={BLOCK} projectId="7" onCloseCanvasEditor={vi.fn()} />),
    );
    await waitFor(() => {
      expect(client.getEmitted('chat_canvas_join')).toHaveLength(1);
    });

    unmount();
    expect(client.getEmitted('chat_canvas_leave_rooms')).toHaveLength(1);
  });

  it('sends the presence heartbeat on the canvas it is editing', async () => {
    const { beats } = installPresenceRoute([]);
    renderWithTheme(
      withSocket(<CanvasEditor selectedCodeBlockInfo={BLOCK} projectId="7" onCloseCanvasEditor={vi.fn()} />),
    );

    await waitFor(() => {
      expect(beats).toHaveLength(1);
    });
    expect(beats[0]).toEqual({
      state: 'editing',
      url: `/api/v2/elitea_core/canvas/prompt_lib/7/${CANVAS_UUID}/presence`,
    });
    // And it subscribed to the PROJECT stream for everybody else's beats.
    expect(registry.getSources()[0]?.url).toBe('/api/v2/elitea_core/events/prompt_lib/7');
  });

  it('renders the other editor and goes read-only for them', async () => {
    installPresenceRoute([{ user_id: '2', user_name: 'grace@example.com' }]);
    const { container } = renderWithTheme(
      withSocket(
        <CanvasEditor
          selectedCodeBlockInfo={BLOCK}
          projectId="7"
          viewer={{ id: '1', name: 'ada@example.com' }}
          onCloseCanvasEditor={vi.fn()}
        />,
      ),
    );

    await waitFor(() => {
      expect(screen.getByTestId('canvas-presence')).toBeInTheDocument();
    });
    expect(screen.getByTestId('canvas-presence')).toHaveTextContent('grace@example.com is editing');
    expect(screen.getAllByTestId('canvas-presence-avatar')).toHaveLength(1);
    // Read-only for everyone who is not in the roster — the reference's rule.
    // Asserted the way CodeMirrorEditor's own suite asserts it: by trying to
    // type. `contenteditable` stays `true` on a CodeMirror in read-only mode;
    // the edit is refused by the state, not by the attribute.
    const user = userEvent.setup();
    await user.click(getContent(container));
    await user.keyboard('X');
    await new Promise((resolve) => setTimeout(resolve, 60));
    expect(getContent(container)).toHaveTextContent('hello');
    expect(getContent(container)).not.toHaveTextContent('Xhello');
  });

  it('renders NOTHING extra when nobody else is on the canvas', async () => {
    const { beats } = installPresenceRoute([{ user_id: '1', user_name: 'ada@example.com' }]);
    const { container } = renderWithTheme(
      withSocket(
        <CanvasEditor
          selectedCodeBlockInfo={BLOCK}
          projectId="7"
          viewer={{ id: '1', name: 'ada@example.com' }}
          onCloseCanvasEditor={vi.fn()}
        />,
      ),
    );

    await waitFor(() => {
      expect(beats).toHaveLength(1);
    });
    expect(screen.queryByTestId('canvas-presence')).toBeNull();
    // Still editable — the "unchanged with no second editor" acceptance
    // criterion, asserted by typing rather than by an attribute.
    const user = userEvent.setup();
    await user.click(getContent(container));
    await user.keyboard('X');
    await waitFor(() => {
      expect(getContent(container)).toHaveTextContent('Xhello');
    });
  });

  /*
   * The defect the chat-stream canvas journey caught. The roster the server
   * answers a beat with ALWAYS holds the caller, so an editor that cannot
   * recognise its own entry reads it as a stranger and locks the only person
   * editing out of their own canvas. The composition root passed no identity
   * at all, and every unit test above supplied one, so nothing here saw it.
   */
  it('stays editable when the roster holds only THIS viewer, matched by id', async () => {
    // A display name the client does not hold — the server names the entry
    // from the principal, which is not what this app renders anywhere.
    const { beats } = installPresenceRoute([{ user_id: '1', user_name: 'ada@corp.internal' }]);
    const { container } = renderWithTheme(
      withSocket(
        <CanvasEditor
          selectedCodeBlockInfo={BLOCK}
          projectId="7"
          viewer={{ id: '1' }}
          onCloseCanvasEditor={vi.fn()}
        />,
      ),
    );

    await waitFor(() => {
      expect(beats).toHaveLength(1);
    });
    expect(screen.queryByTestId('canvas-presence')).toBeNull();
    const user = userEvent.setup();
    await user.click(getContent(container));
    await user.keyboard('X');
    await waitFor(() => {
      expect(getContent(container)).toHaveTextContent('Xhello');
    });
  });

  it('stays editable when the viewer cannot be identified at all', async () => {
    // Fail OPEN. This is not a lock the server enforces, so a client that
    // cannot tell its own entry from a stranger's must not refuse the edit —
    // refusing is how one tab locked itself out.
    const { beats } = installPresenceRoute([{ user_id: '9', user_name: 'someone@example.com' }]);
    const { container } = renderWithTheme(
      withSocket(<CanvasEditor selectedCodeBlockInfo={BLOCK} projectId="7" onCloseCanvasEditor={vi.fn()} />),
    );

    await waitFor(() => {
      expect(beats).toHaveLength(1);
    });
    const user = userEvent.setup();
    await user.click(getContent(container));
    await user.keyboard('X');
    await waitFor(() => {
      expect(getContent(container)).toHaveTextContent('Xhello');
    });
  });

  it('coalesces a burst of keystrokes into ONE remote edit', async () => {
    installPresenceRoute([]);
    /*
     * 60 ms between keystrokes, which matters. `CodeMirrorEditor` already
     * debounces its own `onChange` at 30 ms — the reference's number — so a
     * burst typed faster than that would be coalesced by the HOST and this test
     * would pass with no debounce in the editor at all. Real typing is 150-250
     * ms per keystroke, i.e. every keystroke clears that 30 ms window and sends
     * the WHOLE document. Typing at 60 ms reproduces that at test speed: six
     * separate `notifyChange` calls reach the editor, and the editor's own
     * window is what turns them into one emit.
     */
    const user = userEvent.setup({ delay: 60 });
    const { container } = renderWithTheme(
      withSocket(<CanvasEditor selectedCodeBlockInfo={BLOCK} projectId="7" onCloseCanvasEditor={vi.fn()} />),
    );

    await user.click(getContent(container));
    await user.keyboard('abcdef');

    // Local state keeps up on every keystroke — Save and Copy read it.
    await waitFor(() => {
      expect(getContent(container)).toHaveTextContent('abcdef');
    });

    await waitFor(
      () => {
        expect(client.getEmitted('chat_canvas_edit')).toHaveLength(1);
      },
      { timeout: 5000 },
    );
    const payload = client.getEmitted('chat_canvas_edit')[0]?.payload as { content?: string };
    expect(payload.content).toContain('abcdef');
    // Give the window room to fire again; it must not.
    await new Promise((resolve) => setTimeout(resolve, 600));
    expect(client.getEmitted('chat_canvas_edit')).toHaveLength(1);
  });
});
