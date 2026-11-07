import { act, fireEvent, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { BrowserViewFailure } from '@/shared/api/browserView/client';
import { fakeSocketFactory } from '@/shared/api/browserView/testing';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { CloudBrowserView } from './CloudBrowserView';

const SESSION = 'elitea-br-0123456789abcdef01234567';

function mount(diagnosis: BrowserViewFailure = 'not_found') {
  const fake = fakeSocketFactory();
  const diagnose = vi.fn(() => Promise.resolve({ failure: diagnosis, retryable: diagnosis === 'busy' }));
  renderWithTheme(<CloudBrowserView sessionId={SESSION} streamOptions={{ socketFactory: fake.factory, url: 'ws://host/view', diagnose }} />);
  return { ...fake, diagnose };
}

const drawn: unknown[] = [];

beforeEach(() => {
  drawn.length = 0;
  vi.stubGlobal('createImageBitmap', vi.fn(() => Promise.resolve({ close: () => undefined })));
  vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockImplementation(() => ({ drawImage: (...args: unknown[]) => drawn.push(args) }) as unknown as CanvasRenderingContext2D);
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(() => ({ x: 0, y: 0, left: 0, top: 0, width: 640, height: 400, right: 640, bottom: 400, toJSON: () => ({}) }));
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe('CloudBrowserView', () => {
  it('draws each frame, shows its address and who drives, and acks it once drawn', async () => {
    const { last } = mount();
    expect(screen.getByTestId('cloud-browser-empty')).toBeInTheDocument();
    act(() => {
      last().hello(true);
      last().frame(3, { url: 'https://shop.test/cart', title: 'Cart' });
    });
    await waitFor(() => expect(last().sent).toContainEqual({ type: 'ack', seq: 3 }));
    expect(drawn).toHaveLength(1);
    expect(screen.getByTestId('cloud-browser-address')).toHaveTextContent('https://shop.test/cart');
    expect(screen.getByTestId('cloud-browser-pill')).toHaveTextContent('Agent');
    expect(screen.getByTestId('cloud-browser-canvas')).toHaveAttribute('aria-label', 'Cart');
  });

  it('takes over, sends input in frame pixels, and hands back with a note', async () => {
    const user = userEvent.setup();
    const { last } = mount();
    act(() => {
      last().hello(true);
      last().frame(1);
    });
    await user.click(await screen.findByTestId('cloud-browser-take-over'));
    expect(last().sent).toContainEqual({ type: 'take_over' });

    // The canvas box is 640×400 over a 1280×800 frame: positions double.
    const canvas = screen.getByTestId('cloud-browser-canvas');
    fireEvent.pointerDown(canvas, { clientX: 100, clientY: 50, button: 0, detail: 1 });
    expect(last().sent).toContainEqual({ type: 'pointer', kind: 'down', x: 200, y: 100, button: 1, click_count: 1, modifiers: 0 });
    fireEvent.keyDown(canvas, { key: 'a', code: 'KeyA' });
    expect(last().sent).toContainEqual({ type: 'key', kind: 'char', key: 'a', code: 'KeyA', text: 'a', modifiers: 0 });

    act(() => last().frame(2, { control: 'user' }));
    expect(screen.getByTestId('cloud-browser-pill')).toHaveTextContent('You');
    await user.click(screen.getByTestId('cloud-browser-hand-back'));
    await user.type(screen.getByTestId('cloud-browser-note'), 'signed in, continue');
    await user.click(screen.getByTestId('cloud-browser-hand-back-send'));
    expect(last().sent).toContainEqual({ type: 'hand_back', note: 'signed in, continue' });
    expect(screen.queryByTestId('cloud-browser-hand-back-form')).not.toBeInTheDocument();
  });

  it('is watch-only for a viewer who may not drive: no buttons, no input', async () => {
    const { last } = mount();
    act(() => {
      last().hello(false);
      last().frame(1);
    });
    expect(await screen.findByTestId('cloud-browser-status')).toHaveTextContent('Watching only');
    expect(screen.queryByTestId('cloud-browser-take-over')).not.toBeInTheDocument();
    fireEvent.pointerDown(screen.getByTestId('cloud-browser-canvas'), { clientX: 1, clientY: 1 });
    fireEvent.keyDown(screen.getByTestId('cloud-browser-canvas'), { key: 'a' });
    await waitFor(() => expect(last().sent).toEqual([{ type: 'ack', seq: 1 }]));
  });

  it('opens a lingering session watch-only, whatever Main said about control', () => {
    const { last } = mount();
    act(() => last().hello(true, true));
    expect(screen.getByTestId('cloud-browser-status')).toHaveTextContent('The run ended');
    expect(screen.queryByTestId('cloud-browser-take-over')).not.toBeInTheDocument();
  });

  it('drops to watch-only on control_revoked', async () => {
    const { last } = mount();
    act(() => {
      last().hello(true);
      last().frame(1);
    });
    expect(await screen.findByTestId('cloud-browser-take-over')).toBeInTheDocument();
    act(() => last().text({ type: 'notice', code: 'control_revoked' }));
    expect(screen.queryByTestId('cloud-browser-take-over')).not.toBeInTheDocument();
    expect(screen.getByTestId('cloud-browser-notice')).toHaveTextContent('no longer take over');
  });

  it('shows the end of the session', () => {
    const { last } = mount();
    act(() => {
      last().hello(false);
      last().text({ type: 'end', code: 'wall' });
      last().serverClose(1000, 'wall');
    });
    expect(screen.getByTestId('cloud-browser').dataset['phase']).toBe('ended');
    expect(screen.getByTestId('cloud-browser-status')).toHaveTextContent('time limit');
    expect(screen.getByTestId('cloud-browser-pill')).toHaveTextContent('Ended');
  });

  it('says why a view was refused, and tries again only when it may pass', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { last, sockets } = mount('busy');
    act(() => last().serverClose(1006));
    await waitFor(() => expect(screen.getByTestId('cloud-browser-status')).toHaveTextContent('busy'));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5_000);
    });
    expect(sockets).toHaveLength(2);
  });

  it('does not retry a revoked viewer', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { last, sockets } = mount();
    act(() => {
      last().hello(false);
      last().serverClose(4403, 'access_revoked');
    });
    expect(screen.getByTestId('cloud-browser-status')).toHaveTextContent('no longer see');
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(sockets).toHaveLength(1);
  });
});
