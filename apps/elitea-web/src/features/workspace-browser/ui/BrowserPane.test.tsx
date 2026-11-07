import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { BrowserControlAction, BrowserControlState, BrowserEvent, BrowserIpc, PaneRect } from '@/shared/desktop/browserIpc';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { EMPTY_PANE, foldBrowserEvent, useBrowserPanes } from '../model/browserPane.store';
import { BrowserIpcProvider } from '../model/ipcContext';
import { forgetThreadBrowser } from '../model/useBrowserEvents';
import { modalOpen } from '../model/usePageRect';
import { BrowserDownloadCards } from './BrowserDownloadCards';
import { BrowserPane } from './BrowserPane';

const RECT = { x: 10, y: 20, width: 300, height: 400 };

const done = Promise.resolve();

const IDLE: BrowserControlState = { control: 'idle', reason: null, asked: false, watch: false, deadline_ms: null };

/** The host's control, as `browser_control.rs` answers it (the Rust tests cover the agent's side). */
function hostControl(state: BrowserControlState, action: BrowserControlAction): BrowserControlState {
  switch (action) {
    case 'get':
      return state;
    case 'take_over':
      return state.control === 'idle' ? state : { ...state, control: 'user' };
    case 'hand_back':
      return state.control === 'user' ? { ...state, control: 'agent', reason: null, asked: false, watch: false, deadline_ms: null } : state;
    case 'decline':
      return state.asked ? { ...state, control: 'agent', reason: null, asked: false, deadline_ms: null } : state;
    case 'watch':
      return { ...state, watch: true };
    case 'unwatch':
      return { ...state, watch: false };
  }
}

function fakeIpc(start: BrowserControlState = IDLE) {
  const rects: { rect: PaneRect; visible: boolean }[] = [];
  let state = start;
  const spies = {
    open: vi.fn((_thread: string, address: string) => Promise.resolve({ url: address.includes('://') ? address : `https://${address}/` })),
    setRect: vi.fn((_thread: string, rect: PaneRect, visible: boolean) => {
      rects.push({ rect, visible });
      return done;
    }),
    go: vi.fn(() => done),
    close: vi.fn(() => done),
    forget: vi.fn(() => Promise.resolve(true)),
    revealDownload: vi.fn(() => done),
    control: vi.fn((_thread: string, action: BrowserControlAction, _note?: string) => {
      state = hostControl(state, action);
      return Promise.resolve(state);
    }),
  };
  const ipc: BrowserIpc = { ...spies, onEvent: () => Promise.resolve(() => undefined) };
  // The session page subscribes (`useBrowserEvents`); here an event lands in the store as it would.
  const emit = (event: BrowserEvent): void => useBrowserPanes.getState().apply(event);
  return { ipc, spies, rects, emit };
}

function mount(ipc: BrowserIpc, onClose = vi.fn(), openExternal = vi.fn((_url: string) => done)) {
  renderWithTheme(
    <BrowserIpcProvider ipc={ipc} openExternal={openExternal}>
      <BrowserPane threadId="c1" onClose={onClose} />
      <BrowserDownloadCards threadId="c1" />
    </BrowserIpcProvider>,
  );
  return { onClose, openExternal };
}

beforeEach(() => {
  useBrowserPanes.getState().clear();
  useBrowserPanes.getState().update('c1', { open: true });
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (this: HTMLElement) {
    const box = this.dataset['testid'] === 'browser-page' ? RECT : { x: 0, y: 0, width: 0, height: 0 };
    return { ...box, left: box.x, top: box.y, right: box.x + box.width, bottom: box.y + box.height, toJSON: () => box };
  });
});

afterEach(() => {
  vi.restoreAllMocks();
  document.querySelectorAll('[data-test-modal]').forEach((node) => node.remove());
});

describe('foldBrowserEvent', () => {
  it('follows loads, titles, refusals, downloads and the host closing the page', () => {
    let pane = foldBrowserEvent(EMPTY_PANE, { thread_id: 'c1', kind: 'loading', url: 'https://a.example/' });
    expect(pane).toMatchObject({ live: true, loading: true, url: 'https://a.example/' });
    pane = foldBrowserEvent(pane, { thread_id: 'c1', kind: 'title', title: 'A' });
    pane = foldBrowserEvent(pane, { thread_id: 'c1', kind: 'loaded', url: 'https://a.example/x' });
    expect(pane).toMatchObject({ loading: false, title: 'A', url: 'https://a.example/x' });
    pane = foldBrowserEvent(pane, { thread_id: 'c1', kind: 'refused', url: 'file:///etc/passwd', reason: 'scheme', message: 'No.' });
    expect(pane.refused?.reason).toBe('scheme');
    // A new load clears the refusal.
    expect(foldBrowserEvent(pane, { thread_id: 'c1', kind: 'loading', url: 'https://b.example/' }).refused).toBeNull();
    pane = foldBrowserEvent(pane, { thread_id: 'c1', kind: 'download_started', name: 'r.pdf' });
    pane = foldBrowserEvent(pane, { thread_id: 'c1', kind: 'download_finished', name: 'r.pdf', size: 2048, success: true });
    expect(pane.downloads).toEqual([{ name: 'r.pdf', size: 2048, state: 'done' }]);
    expect(foldBrowserEvent(pane, { thread_id: 'c1', kind: 'closed' })).toMatchObject({ live: false, loading: false });
  });
});

describe('modalOpen', () => {
  it('sees a modal dialog but not a menu or a popover', () => {
    expect(modalOpen(document)).toBe(false);
    const menu = document.createElement('div');
    menu.className = 'MuiModal-root MuiPopover-root MuiMenu-root';
    menu.dataset['testModal'] = '';
    document.body.append(menu);
    expect(modalOpen(document)).toBe(false);
    const dialog = document.createElement('div');
    dialog.setAttribute('aria-modal', 'true');
    dialog.dataset['testModal'] = '';
    document.body.append(dialog);
    expect(modalOpen(document)).toBe(true);
  });
});

describe('BrowserPane', () => {
  it('has every control in a toolbar outside the page box, and the pill', () => {
    mount(fakeIpc().ipc);
    const toolbar = screen.getByTestId('browser-toolbar');
    const page = screen.getByTestId('browser-page');
    for (const id of ['browser-back', 'browser-forward', 'browser-reload', 'browser-address', 'browser-pill', 'browser-open-external', 'browser-more', 'browser-close']) {
      const control = screen.getByTestId(id);
      expect(toolbar.contains(control)).toBe(true);
      expect(page.contains(control)).toBe(false);
    }
    expect(screen.getByTestId('browser-back')).toBeDisabled();
    expect(screen.getByTestId('browser-pill')).toHaveAttribute('data-control', 'idle');
    expect(screen.queryByTestId('browser-take-over')).toBeNull();
  });

  it('opens a typed address over the page rectangle and follows the host’s events', async () => {
    const fake = fakeIpc();
    mount(fake.ipc);
    await userEvent.setup().type(screen.getByTestId('browser-address'), 'example.com{Enter}');
    await waitFor(() => expect(fake.spies.open).toHaveBeenCalledWith('c1', 'example.com', RECT));
    await waitFor(() => expect(screen.getByTestId('browser-address')).toHaveValue('https://example.com/'));
    expect(screen.getByTestId('browser-stop')).toBeInTheDocument();
    act(() => fake.emit({ thread_id: 'c1', kind: 'loaded', url: 'https://example.com/' }));
    await userEvent.setup().click(screen.getByTestId('browser-reload'));
    expect(fake.spies.go).toHaveBeenCalledWith('c1', 'reload');
    await userEvent.setup().click(screen.getByTestId('browser-back'));
    expect(fake.spies.go).toHaveBeenCalledWith('c1', 'back');
  });

  it('shows a refusal above the page, never on it', async () => {
    const fake = fakeIpc();
    mount(fake.ipc);
    act(() => fake.emit({ thread_id: 'c1', kind: 'refused', url: 'http://10.0.0.1/', reason: 'private_network', message: 'Private addresses are not opened.' }));
    const notice = await screen.findByTestId('browser-refused');
    expect(notice).toHaveTextContent('Private addresses are not opened.');
    expect(screen.getByTestId('browser-page').contains(notice)).toBe(false);
  });

  it('hides the page while a modal sheet is open, and when the pane goes away', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const fake = fakeIpc();
    renderWithTheme(
      <BrowserIpcProvider ipc={fake.ipc} openExternal={vi.fn()}>
        <BrowserPane threadId="c1" onClose={vi.fn()} />
      </BrowserIpcProvider>,
    );
    act(() => useBrowserPanes.getState().update('c1', { live: true }));
    const dialog = document.createElement('div');
    dialog.setAttribute('aria-modal', 'true');
    dialog.dataset['testModal'] = '';
    act(() => {
      document.body.append(dialog);
      vi.advanceTimersByTime(600);
    });
    await waitFor(() => expect(fake.rects.at(-1)).toEqual({ rect: RECT, visible: false }));
    act(() => {
      dialog.remove();
      vi.advanceTimersByTime(600);
    });
    await waitFor(() => expect(fake.rects.at(-1)).toEqual({ rect: RECT, visible: true }));
    vi.useRealTimers();
  });

  it('hands the page’s address to the system browser', async () => {
    const fake = fakeIpc();
    const { openExternal } = mount(fake.ipc);
    act(() => fake.emit({ thread_id: 'c1', kind: 'loaded', url: 'https://example.com/a' }));
    await userEvent.setup().click(screen.getByTestId('browser-open-external'));
    expect(openExternal).toHaveBeenCalledWith('https://example.com/a');
  });

  it('closes the page on ×, and Forget site data deletes it', async () => {
    const fake = fakeIpc();
    const { onClose } = mount(fake.ipc);
    const user = userEvent.setup();
    await user.click(screen.getByTestId('browser-close'));
    expect(fake.spies.close).toHaveBeenCalledWith('c1');
    expect(onClose).toHaveBeenCalledTimes(1);
    await user.click(screen.getByTestId('browser-more'));
    await user.click(await screen.findByTestId('browser-forget'));
    await waitFor(() => expect(fake.spies.forget).toHaveBeenCalledWith('c1'));
    await waitFor(() => expect(useBrowserPanes.getState().panes['c1']).toBeUndefined());
  });

  it('shows downloads as cards with Show in Finder once done', async () => {
    const fake = fakeIpc();
    mount(fake.ipc);
    act(() => fake.emit({ thread_id: 'c1', kind: 'download_started', name: 'report.pdf' }));
    expect(await screen.findByTestId('browser-download-card')).toHaveTextContent('Downloading');
    expect(screen.queryByTestId('browser-download-reveal')).toBeNull();
    act(() => fake.emit({ thread_id: 'c1', kind: 'download_finished', name: 'report.pdf', size: 2048, success: true }));
    await userEvent.setup().click(await screen.findByTestId('browser-download-reveal'));
    expect(fake.spies.revealDownload).toHaveBeenCalledWith('c1', 'report.pdf');
  });
});

describe('BrowserPane control pill (#1591)', () => {
  const control = (state: Partial<BrowserControlState>): BrowserEvent => ({ thread_id: 'c1', kind: 'control', ...IDLE, ...state });

  it('reads who drives the page when it mounts, and follows the host’s control events', async () => {
    const fake = fakeIpc({ ...IDLE, control: 'agent' });
    mount(fake.ipc);
    expect(fake.spies.control).toHaveBeenCalledWith('c1', 'get');
    await waitFor(() => expect(screen.getByTestId('browser-pill')).toHaveAttribute('data-control', 'agent'));
    expect(screen.getByTestId('browser-pill')).toHaveTextContent('Agent');
    // The person clicked in the page (the host saw it): You.
    act(() => fake.emit(control({ control: 'user' })));
    expect(screen.getByTestId('browser-pill')).toHaveTextContent('You');
    expect(screen.getByTestId('browser-hand-back')).toBeInTheDocument();
    act(() => fake.emit(control({ control: 'idle' })));
    expect(screen.getByTestId('browser-pill')).toHaveTextContent('Idle');
    expect(screen.queryByTestId('browser-hand-back')).toBeNull();
  });

  it('takes over, lets the agent watch, and hands back with a note', async () => {
    const fake = fakeIpc({ ...IDLE, control: 'agent' });
    mount(fake.ipc);
    const user = userEvent.setup();
    await user.click(await screen.findByTestId('browser-take-over'));
    expect(fake.spies.control).toHaveBeenCalledWith('c1', 'take_over', undefined);
    await waitFor(() => expect(screen.getByTestId('browser-pill')).toHaveAttribute('data-control', 'user'));
    const watch = screen.getByRole('checkbox', { name: 'Let the agent watch' });
    expect(watch).not.toBeChecked();
    await user.click(watch);
    expect(fake.spies.control).toHaveBeenCalledWith('c1', 'watch', undefined);
    await waitFor(() => expect(watch).toBeChecked());
    await user.click(screen.getByTestId('browser-hand-back'));
    await user.type(screen.getByTestId('browser-note'), '  added it to the cart  ');
    await user.click(screen.getByTestId('browser-hand-back-send'));
    expect(fake.spies.control).toHaveBeenLastCalledWith('c1', 'hand_back', 'added it to the cart');
    await waitFor(() => expect(screen.getByTestId('browser-pill')).toHaveAttribute('data-control', 'agent'));
    expect(screen.queryByTestId('browser-hand-back-form')).toBeNull();
    expect(screen.queryByTestId('browser-watch')).toBeNull();
  });

  it('refuses a note over the limit, and Cancel keeps the person driving', async () => {
    const fake = fakeIpc({ ...IDLE, control: 'user' });
    mount(fake.ipc);
    const user = userEvent.setup();
    await user.click(await screen.findByTestId('browser-hand-back'));
    const note = screen.getByTestId('browser-note');
    await user.click(note);
    await user.paste('é'.repeat(1025));
    expect(screen.getByTestId('browser-hand-back-send')).toBeDisabled();
    expect(screen.getByTestId('browser-hand-back-form')).toHaveTextContent('The note is too long.');
    await user.click(screen.getByTestId('browser-hand-back-cancel'));
    expect(screen.queryByTestId('browser-hand-back-form')).toBeNull();
    expect(fake.spies.control).not.toHaveBeenCalledWith('c1', 'hand_back', expect.anything());
    expect(screen.getByTestId('browser-pill')).toHaveAttribute('data-control', 'user');
  });

  it('shows the agent’s request above the page; Done hands back with the answer, Decline declines', async () => {
    const asked = { ...IDLE, control: 'user' as const, asked: true, reason: 'Sign in with your account' };
    const fake = fakeIpc(asked);
    mount(fake.ipc);
    const request = await screen.findByTestId('browser-request');
    expect(request).toHaveTextContent('The agent asks you to use the page: Sign in with your account');
    expect(screen.getByTestId('browser-page').contains(request)).toBe(false);
    const user = userEvent.setup();
    await user.click(screen.getByTestId('browser-request-done'));
    await user.type(screen.getByTestId('browser-note'), 'signed in');
    await user.click(screen.getByTestId('browser-hand-back-send'));
    expect(fake.spies.control).toHaveBeenLastCalledWith('c1', 'hand_back', 'signed in');
    await waitFor(() => expect(screen.queryByTestId('browser-request')).toBeNull());
    // Asked again: Decline.
    act(() => fake.emit(control(asked)));
    await user.click(await screen.findByTestId('browser-decline'));
    expect(fake.spies.control).toHaveBeenLastCalledWith('c1', 'decline', undefined);
  });

  it('shows how long the agent waits, counting down', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const now = Date.now();
    const fake = fakeIpc({ ...IDLE, control: 'user', asked: true, reason: 'Solve the check', deadline_ms: now + 10 * 60_000 });
    mount(fake.ipc);
    expect(await screen.findByTestId('browser-request-deadline')).toHaveTextContent('Declined in 10 min unless you hand back.');
    act(() => {
      vi.advanceTimersByTime(8 * 60_000 + 30_000);
    });
    await waitFor(() => expect(screen.getByTestId('browser-request-deadline')).toHaveTextContent('Declined in 2 min'));
    vi.useRealTimers();
  });

  it('says why a control action failed', async () => {
    const fake = fakeIpc({ ...IDLE, control: 'agent' });
    fake.spies.control.mockImplementation((_thread: string, action: BrowserControlAction) => (action === 'get' ? Promise.resolve({ ...IDLE, control: 'agent' }) : Promise.reject(new Error('The pane is gone.'))));
    mount(fake.ipc);
    await userEvent.setup().click(await screen.findByTestId('browser-take-over'));
    expect(await screen.findByTestId('browser-error')).toHaveTextContent('The pane is gone.');
  });

  it('folds control events, and a closed page keeps nothing stale', () => {
    let pane = foldBrowserEvent(EMPTY_PANE, control({ control: 'user', asked: true, reason: 'MFA', watch: true }));
    expect(pane.control).toEqual({ control: 'user', asked: true, reason: 'MFA', watch: true, deadline_ms: null });
    pane = foldBrowserEvent(pane, control({}));
    expect(pane.control).toEqual(IDLE);
  });
});

describe('forgetThreadBrowser', () => {
  it('drops the pane and asks the host to delete the store, ignoring a host without one', async () => {
    const fake = fakeIpc();
    await forgetThreadBrowser(fake.ipc, 'c1');
    expect(fake.spies.forget).toHaveBeenCalledWith('c1');
    expect(useBrowserPanes.getState().panes['c1']).toBeUndefined();
    await expect(forgetThreadBrowser({ forget: () => Promise.reject(new Error('gone')) }, 'c1')).resolves.toBeUndefined();
    await expect(forgetThreadBrowser(undefined, 'c1')).resolves.toBeUndefined();
  });
});
