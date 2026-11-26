/**
 * Full auto beside Bypass in the composer's modes (issue 3079): picking
 * Bypass leaves Full auto only once Bypass is on (a cancelled first-time
 * warning keeps Full auto), and turning Full auto off returns to Act, not to
 * a Bypass kept for the folder.
 */
import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { clearNamespace } from '@/shared/lib/storage';

import { useComposerModes } from './useComposerModes';
import type { SessionCommands } from './useSessionCommands';

const invoke = vi.fn((command: string) => (command === 'full_auto_status' ? Promise.resolve({ allowed: true }) : Promise.reject(new Error(command))));

function render() {
  const commands = { planMode: false, setPlanMode: vi.fn() } as unknown as SessionCommands;
  return renderHook(() => useComposerModes('w1', true, commands), { wrapper: AppProviders });
}

async function fullAutoOn(view: ReturnType<typeof render>): Promise<void> {
  await waitFor(() => expect(view.result.current.switchProps.fullAuto).toBeDefined());
  act(() => view.result.current.switchProps.fullAuto?.onChange(true));
  act(() => view.result.current.fullAuto.confirm());
  expect(view.result.current.mode).toBe('full_auto');
}

beforeEach(() => {
  clearNamespace();
  vi.stubGlobal('__TAURI_INTERNALS__', { invoke });
});
afterEach(() => vi.unstubAllGlobals());

describe('useComposerModes with Full auto', () => {
  it('keeps Full auto when the Bypass warning is cancelled, and leaves it once Bypass is on', async () => {
    const view = render();
    await fullAutoOn(view);
    act(() => view.result.current.switchProps.onBypassChange?.(true));
    expect(view.result.current.bypass.confirming).toBe(true);
    act(() => view.result.current.bypass.cancel());
    expect(view.result.current.mode).toBe('full_auto');

    act(() => view.result.current.switchProps.onBypassChange?.(true));
    act(() => view.result.current.bypass.confirm());
    expect(view.result.current.mode).toBe('bypass');
  });

  it('returns to Act when Full auto is turned off, even with Bypass kept for the folder', async () => {
    const view = render();
    await waitFor(() => expect(view.result.current.switchProps.fullAuto).toBeDefined());
    act(() => view.result.current.switchProps.onBypassChange?.(true));
    act(() => view.result.current.bypass.confirm());
    expect(view.result.current.mode).toBe('bypass');
    await fullAutoOn(view);
    act(() => view.result.current.fullAuto.request(false));
    expect(view.result.current.mode).toBe('act');
  });
});
