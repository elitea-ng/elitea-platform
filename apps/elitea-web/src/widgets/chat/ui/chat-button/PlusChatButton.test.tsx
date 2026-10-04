/**
 * Pins the drop/paste attachment handle on the "+" menu.
 *
 * `useNewChatInputAttachmentBridge` (features/chat-input) delivers dropped
 * and pasted files exclusively through `attachmentButtonRef.current.onDrop`
 * and silently no-ops while `current` is null. On the chat surface the "+"
 * menu is the composer's left-hand control, and its visible attachment rows
 * live inside a Popper that only mounts while the menu is open — so the
 * handle must attach to the hidden, always-mounted `AttachmentButton`
 * (baseline `PlusChatButton.jsx:313-320`). Before that hidden mount existed,
 * `ref.current` stayed null forever and every file dropped or pasted onto
 * /chat was discarded without any error — which is exactly what these
 * assertions fail on.
 *
 * Harness: `PlusChatButton` reads `agentEditorHooks.useAvailableInternalTools`
 * (router context via `useSelectedProjectId`) and `useIsMcpVisible` (the real
 * `GET /elitea_core/platform_settings/prompt_lib` query), so the mount gets a
 * memory router + QueryClient and the settings endpoint is substituted at the
 * network boundary (MSW, per R-M1) — same ingredients as
 * `features/chat-input/ui/NewChatInput.test.tsx`'s own harness, minus the
 * socket (nothing here mounts the voice loop).
 */
import { createRef } from 'react';
import type { ReactElement, RefObject } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { act, render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { remToPx } from '@/shared/ui/lib/testTheme';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { PlusChatButton } from './PlusChatButton';
import type { AttachmentButtonHandle } from './AttachmentButton';

const BASE = '/api/v2';
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

afterEach(() => {
  resetGeneratedClient();
});

async function renderPlusButton(props: {
  readonly attachmentButtonRef: RefObject<AttachmentButtonHandle | null>;
  readonly onAttachFiles: (files: readonly File[]) => void;
}): Promise<{ readonly container: HTMLElement }> {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/platform_settings/prompt_lib`, () => HttpResponse.json({ mcp_enabled: true, mcp_in_menu_enabled: true })),
  );

  function RootComponent(): ReactElement {
    return (
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}>
        <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
          <PlusChatButton attachmentButtonRef={props.attachmentButtonRef} onAttachFiles={props.onAttachFiles} />
        </ThemeProvider>
      </QueryClientProvider>
    );
  }
  const router = createRouter({
    routeTree: createRootRoute({ component: RootComponent }),
    history: createMemoryHistory({ initialEntries: ['/'] }),
    // Toolkit-schema queries stay disabled on an undefined project id
    // (`useToolkitTypeSchemas`'s own `enabled` gate), so no handler is needed.
    context: { auth: { getSelectedProjectId: () => undefined } },
  });
  const view = render(<RouterProvider router={router} />);
  await waitFor(() => {
    expect(screen.getByTestId('plus-menu-button')).toBeInTheDocument();
  });
  return { container: view.container };
}

describe('PlusChatButton — drop/paste attachment handle', () => {
  it('exposes the imperative onDrop handle while the menu is CLOSED', async () => {
    const ref = createRef<AttachmentButtonHandle | null>();
    await renderPlusButton({ attachmentButtonRef: ref, onAttachFiles: vi.fn() });

    // The menu has never been opened — the Popper rows don't exist. The
    // handle must be live anyway, or every drop/paste is a silent no-op.
    expect(ref.current).not.toBeNull();
  });

  it('dispatches dropped files through the handle to onAttachFiles', async () => {
    const ref = createRef<AttachmentButtonHandle | null>();
    const onAttachFiles = vi.fn<(files: readonly File[]) => void>();
    await renderPlusButton({ attachmentButtonRef: ref, onAttachFiles });

    const file = new File(['hello'], 'notes.txt', { type: 'text/plain' });
    act(() => {
      ref.current?.onDrop({ dataTransfer: { files: [file] }, preventDefault: () => {} });
    });

    expect(onAttachFiles).toHaveBeenCalledTimes(1);
    expect(onAttachFiles.mock.calls[0]?.[0]).toEqual([file]);
  });

  /**
   * The same always-mounted instance, seen from the page.
   *
   * It used to render the normal icon button inside a 0x0,
   * `pointer-events: none` Box. That put a permanently dead "attach files"
   * button in the accessibility tree, and with the menu closed it was the ONLY
   * match for that name — so a screen reader, a test or an agent driving the
   * browser found the dead control first, clicked it, got nothing, and
   * concluded attachments were broken. Hiding it with `aria-hidden` merely
   * traded that for an axe `aria-hidden-focus` violation, because it stayed
   * focusable. `dropTargetOnly` renders no DOM for it at all.
   */
  it('gives the always-mounted drop target no DOM of its own', async () => {
    const ref = createRef<AttachmentButtonHandle | null>();
    const { container } = await renderPlusButton({ attachmentButtonRef: ref, onAttachFiles: vi.fn() });

    expect(screen.queryByRole('button', { name: 'attach files' })).toBeNull();
    expect(container.querySelector('input[type="file"]')).toBeNull();
    // …and the handle it exists for is live anyway.
    expect(ref.current).not.toBeNull();
  });
});

/**
 * #6629: every submenu opens beside the main menu, aligned with its top edge,
 * and has one width — whichever category row opened it. jsdom has no layout,
 * so the rows and the menu paper get fake boxes at different heights: a
 * submenu anchored to the hovered ROW would move between the two categories.
 */
describe('PlusChatButton — submenu position and size (#6629)', () => {
  const realRect = Object.getOwnPropertyDescriptor(Element.prototype, 'getBoundingClientRect');
  const TOPS: Record<string, number> = { 'plus-menu-tools': 90, 'plus-menu-agents': 134, 'plus-menu-pipelines': 178 };

  afterEach(() => {
    if (realRect) Object.defineProperty(Element.prototype, 'getBoundingClientRect', realRect);
  });

  function fakeLayout(): void {
    Element.prototype.getBoundingClientRect = function fakeRect(this: Element): DOMRect {
      const owner = this.closest('[data-testid]');
      const top = TOPS[owner?.getAttribute('data-testid') ?? ''] ?? 0;
      return DOMRect.fromRect({ x: 20, y: top, width: 280, height: 40 });
    };
  }

  function submenuPopper(): HTMLElement {
    // MUI's Popper root carries role="tooltip".
    const popper = screen.getByTestId('plus-submenu-paper').closest<HTMLElement>('[role="tooltip"]');
    if (!popper) throw new Error('the submenu is not in a Popper');
    return popper;
  }

  it('opens every category at the same place, at one fixed width', async () => {
    await renderPlusButton({ attachmentButtonRef: createRef<AttachmentButtonHandle | null>(), onAttachFiles: vi.fn() });
    fakeLayout();
    act(() => {
      screen.getByTestId('plus-menu-button').click();
    });
    await waitFor(() => expect(screen.getByTestId('plus-menu-agents')).toBeInTheDocument());

    act(() => {
      screen.getByTestId('plus-menu-agents').dispatchEvent(new MouseEvent('mouseover', { bubbles: true }));
    });
    await waitFor(() => expect(submenuPopper().style.transform).not.toBe(''));
    const agentsTransform = submenuPopper().style.transform;
    expect(screen.getByTestId('plus-submenu-paper')).toHaveStyle({ width: remToPx('17.5rem') });

    act(() => {
      screen.getByTestId('plus-menu-pipelines').dispatchEvent(new MouseEvent('mouseover', { bubbles: true }));
    });
    await waitFor(() => expect(submenuPopper().style.transform).not.toBe(''));
    expect(submenuPopper().style.transform).toBe(agentsTransform);
    // Not a row top (134 or 178): the submenu follows the menu, not the row.
    expect(agentsTransform).not.toMatch(/134px|178px/);
  });
});
