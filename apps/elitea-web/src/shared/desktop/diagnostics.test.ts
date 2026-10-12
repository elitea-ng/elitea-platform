import { afterEach, describe, expect, it, vi } from 'vitest';

import { createHostLogger, hostLog, installDiagnostics, pathOnly, scrubLogText, withRequestLogging, type HostLogger } from './diagnostics';

describe('scrubLogText', () => {
  it('redacts bearer tokens and Authorization values', () => {
    expect(scrubLogText('Authorization: Bearer eyJhbGciOi.payload.sig')).toBe('Authorization: [redacted]');
    expect(scrubLogText('{"Authorization":"Bearer abc","x":1}')).toBe('{"Authorization":"[redacted]","x":1}');
    expect(scrubLogText('retrying with bearer abc.def-ghi now')).toBe('retrying with bearer [redacted] now');
  });

  it('redacts credential parameters in form, query and JSON notation', () => {
    expect(scrubLogText('grant_type=refresh_token&refresh_token=r-123&client_id=x')).toBe(
      'grant_type=refresh_token&refresh_token=[redacted]&client_id=x',
    );
    expect(scrubLogText('{"access_token":"a1","expires_in":900,"refresh_token":"r1"}')).toBe(
      '{"access_token":"[redacted]","expires_in":900,"refresh_token":"[redacted]"}',
    );
    expect(scrubLogText('code: abc123')).toBe('code: [redacted]');
    expect(scrubLogText("password='hunter2'")).toBe("password='[redacted]'");
  });

  it('drops the query string and fragment of every URL', () => {
    expect(scrubLogText('GET http://127.0.0.1:53111/callback?code=c&state=s failed')).toBe(
      'GET http://127.0.0.1:53111/callback?[redacted] failed',
    );
    expect(scrubLogText('see https://elitea.example.com/app#access_token=t')).toBe('see https://elitea.example.com/app?[redacted]');
  });

  it('leaves ordinary text, statuses and words that merely contain a key alone', () => {
    const text = 'GET /api/v2/projects -> 500 in 120ms; statuscode=500 encode=utf-8 tokens: 3';
    expect(scrubLogText(text)).toBe(text);
  });
});

describe('pathOnly', () => {
  it('keeps origin and path, never the query or fragment', () => {
    expect(pathOnly('https://elitea.example.com/api/v2/x?token=t#f')).toBe('https://elitea.example.com/api/v2/x');
    expect(pathOnly('/auth/info?x=1')).toBe('/auth/info');
  });
});

describe('createHostLogger', () => {
  it('sends a scrubbed, capped message through plugin:log|log', async () => {
    const invoke = vi.fn().mockResolvedValue(undefined);
    createHostLogger(invoke).log('warn', `Bearer secret ${'x'.repeat(5000)}`, 'test');
    expect(invoke).toHaveBeenCalledTimes(1);
    const [command, args] = invoke.mock.calls[0] as [string, { level: number; message: string; location: string }];
    expect(command).toBe('plugin:log|log');
    expect(args.level).toBe(4);
    expect(args.location).toBe('test');
    expect(args.message.startsWith('Bearer [redacted]')).toBe(true);
    expect(args.message.endsWith('…[truncated]')).toBe(true);
    await Promise.resolve();
  });

  it('swallows a failed write', async () => {
    const invoke = vi.fn().mockRejectedValue(new Error('denied'));
    expect(() => createHostLogger(invoke).log('info', 'x')).not.toThrow();
    await Promise.resolve();
  });
});

describe('installDiagnostics', () => {
  let uninstall: (() => void) | undefined;
  afterEach(() => {
    uninstall?.();
    uninstall = undefined;
  });

  function recorder(): { logger: HostLogger; lines: [string, string, string | undefined][] } {
    const lines: [string, string, string | undefined][] = [];
    return { lines, logger: { log: (level, message, location) => lines.push([level, message, location]) } };
  }

  it('forwards console.error/warn, window errors and unhandled rejections, still calling the console', () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    const { logger, lines } = recorder();
    uninstall = installDiagnostics(logger);

    console.error('boom', new Error('bad'));
    console.warn('careful');
    window.dispatchEvent(new ErrorEvent('error', { message: 'uncaught', filename: 'tauri://localhost/assets/a.js?v=1', lineno: 3, colno: 4 }));
    const rejection = new Event('unhandledrejection') as PromiseRejectionEvent;
    Object.defineProperty(rejection, 'reason', { value: 'nope' });
    window.dispatchEvent(rejection);

    expect(errorSpy).toHaveBeenCalledWith('boom', expect.any(Error));
    expect(warnSpy).toHaveBeenCalledWith('careful');
    expect(lines.map(([level, , location]) => `${level}:${location ?? ''}`)).toEqual([
      'error:console',
      'warn:console',
      'error:window.error',
      'error:unhandledrejection',
    ]);
    expect(lines[0]?.[1]).toContain('Error: bad');
    expect(lines[2]?.[1]).toBe('uncaught at tauri://localhost/assets/a.js:3:4');
  });

  it('makes hostLog a no-op once uninstalled', () => {
    const { logger, lines } = recorder();
    uninstall = installDiagnostics(logger);
    hostLog('debug', 'one');
    uninstall();
    uninstall = undefined;
    hostLog('debug', 'two');
    expect(lines.map(([, message]) => message)).toEqual(['one']);
  });
});

describe('withRequestLogging', () => {
  let uninstall: (() => void) | undefined;
  afterEach(() => {
    uninstall?.();
    uninstall = undefined;
  });

  it('logs failed and slow requests by method, path and status — never the query', async () => {
    const lines: string[] = [];
    uninstall = installDiagnostics({ log: (_level, message) => lines.push(message) });
    let clock = 0;
    const responses = [new Response(null, { status: 200 }), new Response(null, { status: 401 }), new Response(null, { status: 200 })];
    const fetchImpl = vi.fn((_input: string | URL | Request, _init?: RequestInit) => {
      clock += lines.length === 1 ? 2500 : 10;
      return Promise.resolve(responses.shift() ?? new Response(null, { status: 200 }));
    });
    const logged = withRequestLogging(fetchImpl, () => clock);

    await logged('https://elitea.example.com/api/v2/fast?token=t');
    await logged('https://elitea.example.com/api/v2/denied?code=c', { method: 'post' });
    await logged('https://elitea.example.com/api/v2/slow');
    await expect(withRequestLogging((_input: string | URL | Request) => Promise.reject<Response>(new TypeError('offline')), () => 0)('https://e.example/x?q=1')).rejects.toThrow('offline');

    expect(lines).toEqual([
      'POST https://elitea.example.com/api/v2/denied -> 401 in 10ms',
      'GET https://elitea.example.com/api/v2/slow -> 200 in 2500ms',
      expect.stringMatching(/^GET https:\/\/e\.example\/x -> failed in 0ms: TypeError: offline/),
    ]);
  });
});
