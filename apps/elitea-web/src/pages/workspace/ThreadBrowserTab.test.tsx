import { screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { useBrowserPanes } from '@/features/workspace-browser';
import { setNativeTransport, type NativeTransport } from '@/shared/api/nativeTransport';
import { fakeSocketFactory } from '@/shared/api/browserView/testing';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { SidePanelHarness } from '@/test/sidePanelHarness';

import { ThreadBrowserTab } from './ThreadBrowserTab';

const SESSION = 'elitea-br-0123456789abcdef01234567';

afterEach(() => {
  setNativeTransport(undefined);
  useBrowserPanes.getState().update('42', { open: false });
  vi.restoreAllMocks();
});

describe("a Local work thread's Browser tab", () => {
  it("streams the cloud run's browser in the Browser tab for a thread in the cloud", async () => {
    const fake = fakeSocketFactory();
    const native: NativeTransport = { accessToken: () => Promise.resolve('t'), refresh: () => Promise.resolve('refreshed'), signOut: () => undefined, origin: 'https://deploy.example', createWebSocket: fake.factory };
    setNativeTransport(native);
    renderWithTheme(
      <SidePanelHarness>
        <ThreadBrowserTab conversationId="42" cloudSession={SESSION} subject="Thread" />
      </SidePanelHarness>,
    );
    await waitFor(() => expect(screen.getByTestId('cloud-browser')).toBeInTheDocument());
    expect(screen.getByTestId('side-panel-tab-browser')).toContainElement(screen.getByTestId('cloud-browser'));
    expect(fake.last().url).toContain(`/browser_sessions/${SESSION}/view`);
  });

  it('offers no tab for a cloud thread whose run has not used the browser', () => {
    renderWithTheme(
      <SidePanelHarness>
        <ThreadBrowserTab conversationId="42" cloudSession={null} subject="Thread" />
      </SidePanelHarness>,
    );
    expect(screen.queryByTestId('side-panel-tab-browser')).toBeNull();
  });

  it('is the local pane, while open, for a thread on this computer; none for a new thread', () => {
    const { rerender } = renderWithTheme(
      <SidePanelHarness>
        <ThreadBrowserTab conversationId="" cloudSession={undefined} subject="Thread" />
      </SidePanelHarness>,
    );
    expect(screen.queryByTestId('side-panel-tab-browser')).toBeNull();
    useBrowserPanes.getState().update('42', { open: true });
    rerender(
      <SidePanelHarness>
        <ThreadBrowserTab conversationId="42" cloudSession={undefined} subject="Thread" />
      </SidePanelHarness>,
    );
    expect(screen.getByTestId('side-panel-tab-browser')).toBeInTheDocument();
    expect(screen.queryByTestId('cloud-browser')).toBeNull();
  });
});
