import { afterEach, describe, expect, it, vi } from 'vitest';

import { renderHook, waitFor } from '@testing-library/react';

import type { SessionCommands } from './useSessionCommands';
import { useRetryAndSteps, type SentMessage } from './useRetryAndSteps';

function fakeCommands(): SessionCommands & { shown: string[]; nothing: number } {
  const fake = {
    retryRequest: 0,
    shown: [] as string[],
    nothing: 0,
    showSteps: (text: string) => fake.shown.push(text),
    showNothingToRetry: () => {
      fake.nothing += 1;
    },
  };
  return fake as unknown as SessionCommands & { shown: string[]; nothing: number };
}

/** The host's agent settings, as `workspace_agent_settings_get` / `_set` keep them. */
function fakeHost(initial: Record<string, unknown>) {
  let settings = { ...initial };
  const invoke = vi.fn((command: string, args: { settings?: Record<string, unknown> }) => {
    if (command === 'workspace_agent_settings_set' && args.settings !== undefined) settings = args.settings;
    return Promise.resolve(settings);
  });
  (globalThis as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__ = { invoke };
  return { invoke, settings: () => settings };
}

describe('useRetryAndSteps', () => {
  afterEach(() => {
    delete (globalThis as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__;
  });

  /** The hook under `commands` whose `retryRequest` the test bumps, as `/retry` and a failed turn's Retry do. */
  function retrying(send: ReturnType<typeof vi.fn>, busy: boolean, recorded: SentMessage | null) {
    const commands = fakeCommands();
    const hook = renderHook(({ request }: { request: number }) => useRetryAndSteps('w1', { ...commands, retryRequest: request }, vi.fn(), send as never, busy, recorded), { initialProps: { request: 0 } });
    return { commands, hook, ask: (request: number) => hook.rerender({ request }) };
  }

  it('a retry request sends the last message again as it was sent, and the recorded prompt before any send', async () => {
    const send = vi.fn(() => Promise.resolve(true));
    const { hook, ask } = retrying(send, false, { prompt: 'from history', mentions: ['a.txt'], skills: [] });
    // Nothing is sent before a request.
    expect(send).not.toHaveBeenCalled();
    ask(1);
    expect(send).toHaveBeenLastCalledWith('from history', ['a.txt'], [], undefined);

    await hook.result.current.remembering(send)('/style fix it', ['b.txt'], ['style'], undefined, { picks: [{ token: 'img' }], shown: [] } as never);
    ask(2);
    // Without its images: their tokens were spent by the first send.
    expect(send).toHaveBeenLastCalledWith('/style fix it', ['b.txt'], ['style'], undefined);
    // A re-render with the same request sends nothing more.
    ask(2);
    expect(send).toHaveBeenCalledTimes(3);
  });

  it('a retry request says so when there is nothing to send, and waits while a turn runs', () => {
    const send = vi.fn(() => Promise.resolve(true));
    const idle = retrying(send, false, null);
    idle.ask(1);
    expect(idle.commands.nothing).toBe(1);
    const busy = retrying(send, true, { prompt: 'p', mentions: [], skills: [] });
    busy.ask(1);
    expect(send).not.toHaveBeenCalled();
  });

  it('passes /retry to the session, which turns it into a request', () => {
    const run = vi.fn();
    const { result } = renderHook(() => useRetryAndSteps('w1', fakeCommands(), run, vi.fn(), false, null));
    result.current.onCommand('retry');
    expect(run).toHaveBeenCalledWith('retry');
  });

  it('passes the other commands on', () => {
    const run = vi.fn();
    const { result } = renderHook(() => useRetryAndSteps('w1', fakeCommands(), run, vi.fn(), false, null));
    result.current.onCommand('plan');
    expect(run).toHaveBeenCalledWith('plan');
  });

  it('/steps shows the limit and /steps <value> sets it, keeping the other settings', async () => {
    const host = fakeHost({ step_limit: null, diagnostics: false });
    const commands = fakeCommands();
    const { result } = renderHook(() => useRetryAndSteps('w1', commands, vi.fn(), vi.fn(), false, null));

    result.current.onCommand('steps');
    await waitFor(() => expect(commands.shown[0]).toContain('1024 model steps (the default)'));

    result.current.onStepsCommand({ limit: 'unlimited' });
    await waitFor(() => expect(commands.shown[1]).toContain('no step limit'));
    expect(host.settings()).toEqual({ step_limit: 'unlimited', diagnostics: false });

    result.current.onStepsCommand({ limit: 300 });
    await waitFor(() => expect(commands.shown[2]).toContain('300 model steps'));

    result.current.onStepsCommand({ invalid: 'many' });
    expect(commands.shown[3]).toContain('1 to 1000000');
    expect(host.invoke).toHaveBeenCalledWith('workspace_agent_settings_set', { id: 'w1', settings: { step_limit: 300, diagnostics: false } });
  });
});
