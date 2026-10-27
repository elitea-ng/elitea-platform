import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';

import { DEFAULT_COMPACTION_SETTINGS, installCompactionIpc, type CompactionSettings } from '@/shared/desktop/compactionSettings';
import { createFakeCompactionIpc } from '@/shared/desktop/compactionSettings.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { AutoCompactionSection } from './AutoCompactionSection';

function setup(over: Partial<CompactionSettings> = {}) {
  const ipc = createFakeCompactionIpc({ ...DEFAULT_COMPACTION_SETTINGS, ...over });
  installCompactionIpc(ipc);
  renderWithTheme(<AutoCompactionSection />);
  return { ipc, user: userEvent.setup() };
}

const combobox = (name: RegExp) => screen.getByRole('combobox', { name });

async function pick(user: ReturnType<typeof userEvent.setup>, name: RegExp, option: RegExp): Promise<void> {
  await user.click(combobox(name));
  await user.click(within(screen.getByRole('listbox')).getByRole('option', { name: option }));
}

describe('AutoCompactionSection', () => {
  afterEach(() => installCompactionIpc(undefined));

  it('shows the host’s settings, app-wide', async () => {
    setup({ threshold_percent: 80, idle: 'cache_1h' });
    expect(await screen.findByRole('switch', { name: 'Compact automatically' })).toBeChecked();
    expect(combobox(/Compact when context reaches/)).toHaveTextContent('80%');
    expect(combobox(/When idle/)).toHaveTextContent('Before the 1-hour cache expires (55 min) · cache writes cost 2×');
    expect(screen.getByText('For all folders.')).toBeInTheDocument();
    expect(screen.getByText(/Only threads with 60k\+ tokens/)).toBeInTheDocument();
  });

  it('writes each change to the host', async () => {
    const { ipc, user } = setup();
    await screen.findByRole('switch', { name: 'Compact automatically' });
    expect(combobox(/Compact when context reaches/)).toHaveTextContent('Near the limit');
    await pick(user, /Compact when context reaches/, /^60%$/);
    await waitFor(() => expect(ipc.current().threshold_percent).toBe(60));
    await pick(user, /When idle/, /^Off$/);
    await waitFor(() => expect(ipc.current()).toEqual({ auto: true, threshold_percent: 60, idle: 'off', idle_minutes: null }));
    await pick(user, /Compact when context reaches/, /Near the limit/);
    await waitFor(() => expect(ipc.current().threshold_percent).toBeNull());
  });

  it('turns the choices off with the switch', async () => {
    const { ipc, user } = setup();
    await user.click(await screen.findByRole('switch', { name: 'Compact automatically' }));
    await waitFor(() => expect(ipc.current().auto).toBe(false));
    expect(combobox(/Compact when context reaches/)).toHaveAttribute('aria-disabled', 'true');
    expect(combobox(/When idle/)).toHaveAttribute('aria-disabled', 'true');
  });

  it('sets custom minutes and says what they cost', async () => {
    const { ipc, user } = setup();
    await screen.findByRole('switch', { name: 'Compact automatically' });
    await pick(user, /When idle/, /Custom/);
    await waitFor(() => expect(ipc.current()).toMatchObject({ idle: 'custom', idle_minutes: 30 }));
    const minutes = screen.getByTestId('auto-compaction-minutes');
    expect(screen.getByTestId('auto-compaction-minutes-note')).toHaveTextContent('Uses the 1-hour cache: cache writes cost 2×');

    await user.clear(minutes);
    await user.type(minutes, '90');
    expect(screen.getByTestId('auto-compaction-minutes-note')).toHaveTextContent('Later than the cache lasts: idle compaction won’t run');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(ipc.current().idle_minutes).toBe(90));

    await user.clear(minutes);
    await user.type(minutes, '3');
    await user.tab();
    await waitFor(() => expect(ipc.current().idle_minutes).toBe(3));
    expect(screen.queryByTestId('auto-compaction-minutes-note')).toBeNull();

    await user.clear(minutes);
    await user.type(minutes, '700');
    await user.tab();
    expect(screen.getByTestId('auto-compaction-minutes-note')).toHaveTextContent('From 1 to 600 minutes.');
    expect(ipc.current().idle_minutes).toBe(3);
  });

  it('follows a change made elsewhere, and says why the host refused one', async () => {
    const { ipc, user } = setup();
    await screen.findByRole('switch', { name: 'Compact automatically' });
    ipc.change({ ...DEFAULT_COMPACTION_SETTINGS, threshold_percent: 95 });
    await waitFor(() => expect(combobox(/Compact when context reaches/)).toHaveTextContent('95%'));

    ipc.set = () => Promise.reject(new Error('The host is busy.'));
    await pick(user, /Compact when context reaches/, /^40%$/);
    expect(await screen.findByRole('alert')).toHaveTextContent('The host is busy.');
    expect(combobox(/Compact when context reaches/)).toHaveTextContent('95%');
  });

  it('shows nothing outside the desktop host', () => {
    renderWithTheme(<AutoCompactionSection />);
    expect(screen.queryByTestId('auto-compaction')).toBeNull();
  });
});
