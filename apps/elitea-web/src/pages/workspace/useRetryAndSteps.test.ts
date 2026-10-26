import { afterEach, describe, expect, it, vi } from 'vitest';

import { renderHook, waitFor } from '@testing-library/react';

import type { SessionCommands } from './useSessionCommands';
import { CONTINUE_PROMPT, useRetryAndSteps, type SentMessage, type Sending } from './useRetryAndSteps';

function fakeCommands(): SessionCommands & { shown: string[] } {
  const fake = {
    retryRequest: 0,
    continueRequest: 0,
    shown: [] as string[],
    showText: (text: string) => fake.shown.push(text),
  };
  return fake as unknown as SessionCommands & { shown: string[] };
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

/** The hook under `commands` whose requests the test bumps, as `/retry`, Retry and Continue do. */
function requesting(sending: Partial<Sending>, recorded: SentMessage | null) {
  const commands = fakeCommands();
  const send = vi.fn(() => Promise.resolve(true));
  const all: Sending = { send, busy: false, canSend: true, ...sending };
  const hook = renderHook(({ retry, more }: { retry: number; more: number }) => useRetryAndSteps('w1', { ...commands, retryRequest: retry, continueRequest: more }, vi.fn(), all, recorded), {
    initialProps: { retry: 0, more: 0 },
  });
  return { commands, send, hook, ask: (retry: number, more = 0) => hook.rerender({ retry, more }) };
}

describe('useRetryAndSteps', () => {
  afterEach(() => {
    delete (globalThis as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__;
  });

  it('a retry request sends the last message again as it was sent, and the recorded one before any send', async () => {
    const { send, hook, ask } = requesting({}, { prompt: '/style tidy', mentions: ['a.txt'], skills: ['style'] });
    expect(send).not.toHaveBeenCalled();
    ask(1);
    expect(send).toHaveBeenLastCalledWith('/style tidy', ['a.txt'], ['style'], undefined);

    await hook.result.current.remembering(send)('/style fix it', ['b.txt'], ['style'], undefined, { picks: [{ token: 'img' }], shown: [] } as never);
    ask(2);
    // Without its images: their tokens were spent by the first send.
    expect(send).toHaveBeenLastCalledWith('/style fix it', ['b.txt'], ['style'], undefined);
    // A re-render with the same request sends nothing more.
    ask(2);
    expect(send).toHaveBeenCalledTimes(3);
  });

  it('a continue request starts a turn from the host prompt', () => {
    const { send, ask } = requesting({}, null);
    ask(0, 1);
    expect(send).toHaveBeenCalledWith(CONTINUE_PROMPT, [], [], undefined);
  });

  it('says why nothing went out: no message, a running turn, or a composer that may not send', () => {
    const none = requesting({}, null);
    none.ask(1);
    expect(none.commands.shown).toEqual(['There is no message here to send again.']);

    const message = { prompt: 'p', mentions: [], skills: [] };
    const busy = requesting({ busy: true }, message);
    busy.ask(1);
    busy.ask(1, 1);
    expect(busy.send).not.toHaveBeenCalled();
    expect(busy.commands.shown).toHaveLength(2);
    expect(busy.commands.shown[0]).toContain('still running');

    const away = requesting({ canSend: false }, message);
    away.ask(1);
    expect(away.send).not.toHaveBeenCalled();
    expect(away.commands.shown[0]).toContain('Nothing can be sent here now');
  });

  it('passes /retry to the session, which turns it into a request', () => {
    const run = vi.fn();
    const { result } = renderHook(() => useRetryAndSteps('w1', fakeCommands(), run, { send: vi.fn(), busy: false, canSend: true }, null));
    result.current.onCommand('retry');
    expect(run).toHaveBeenCalledWith('retry');
  });

  it('/steps shows the limit and /steps <value> sets it, keeping the other settings', async () => {
    const host = fakeHost({ step_limit: null, diagnostics: false });
    const commands = fakeCommands();
    const { result } = renderHook(() => useRetryAndSteps('w1', commands, vi.fn(), { send: vi.fn(), busy: false, canSend: true }, null));

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
