import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { IndexStatus } from '@/shared/desktop/indexIpc';
import { createFakeIndexIpc } from '@/shared/desktop/indexIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { IndexIpcProvider } from '../model/ipcContext';
import { IndexStatusIcon } from './IndexStatusIcon';

const READY: Partial<IndexStatus> = { state: 'ready', files: 120, entities: 12_400, relations: 3_000, last_run: '2026-10-10T08:00:00Z' };

function mount(status: Partial<IndexStatus>, onOpen = vi.fn()) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={client}>
      <IndexIpcProvider ipc={createFakeIndexIpc({ statuses: { w1: status } })}>
        <IndexStatusIcon workspaceId="w1" onOpen={onOpen} />
      </IndexIpcProvider>
    </QueryClientProvider>,
  );
  return onOpen;
}

const icon = (): HTMLElement => screen.getByTestId('index-status');

describe('IndexStatusIcon', () => {
  it.each([
    [READY, 'quiet', /Index ready · 12(\.4)?K entities/],
    [{ state: 'building' }, 'busy', 'Index building…'],
    [{ state: 'error', error: 'disk full' }, 'error', 'Index error'],
    [{ state: 'off' }, 'quiet', /Index off/],
  ] satisfies [Partial<IndexStatus>, string, string | RegExp][])('says %o with its colour and in words', async (status, tone, label) => {
    mount(status);
    await waitFor(() => expect(icon()).toHaveAccessibleName(label));
    expect(icon()).toHaveAttribute('data-tone', tone);
  });

  it('opening a stale index refreshes it: the icon says it is building', async () => {
    mount({ ...READY, state: 'stale', changed_files: 3 });
    await waitFor(() => expect(icon()).toHaveAttribute('data-tone', 'busy'));
  });

  it('opens the Index tab on a click', async () => {
    const onOpen = mount(READY);
    await userEvent.setup().click(await screen.findByTestId('index-status'));
    expect(onOpen).toHaveBeenCalledTimes(1);
  });
});
