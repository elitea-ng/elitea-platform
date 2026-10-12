/**
 * Desktop diagnostics: the webview's half of the host log file (README,
 * "Logs"). Messages go to `tauri-plugin-log` through `plugin:log|log` — the
 * one log permission the capability grants; the page can write, never read.
 *
 * Forwarded: `console.error` / `console.warn`, window `error` and
 * `unhandledrejection`, and — at debug — route transitions and failed native
 * HTTP requests. Every message is scrubbed here first ({@link scrubLogText});
 * the host scrubs again on the way to the file.
 *
 * Outside the desktop webview no logger is installed and every call is a
 * no-op, so shared code may call {@link hostLog} unconditionally.
 */
import type { HostInvoke } from './hostBridge';

/** `tauri-plugin-log`'s `LogLevel` (serialised as integers). */
const LEVEL = { trace: 1, debug: 2, info: 3, warn: 4, error: 5 } as const;
export type LogLevel = keyof typeof LEVEL;

/** One message is capped: a stack or a response body is not worth megabytes of file. */
const MAX_MESSAGE_CHARS = 4000;

const SECRET_KEYS = [
  'access_token',
  'refresh_token',
  'id_token',
  'token',
  'code',
  'code_verifier',
  'client_secret',
  'password',
] as const;

const URL_WITH_QUERY = /\b([a-z][a-z0-9+.-]*:\/\/[^\s"'<>)\]}?#]*)[?#][^\s"'<>)\]}]*/gi;
const BEARER = /\b(bearer)\s+[^\s"',;]+/gi;
const AUTHORIZATION = /\b(authorization["']?\s*[:=]\s*["']?)[^"'\n,;}]+/gi;
const SECRET_PARAM = new RegExp(`(^|[^A-Za-z0-9_])(${SECRET_KEYS.join('|')})(["']?\\s*[:=]\\s*["']?)[^\\s"'&,;}\\])]+`, 'gi');

/**
 * Redact what must never reach a log file: bearer tokens, `Authorization`
 * values, credential parameters in query, form or JSON notation, and the
 * query string or fragment of any URL (sign-in codes and tokens travel there).
 */
export function scrubLogText(input: string): string {
  return input
    .replace(URL_WITH_QUERY, '$1?[redacted]')
    .replace(AUTHORIZATION, '$1[redacted]')
    .replace(BEARER, '$1 [redacted]')
    .replace(SECRET_PARAM, '$1$2$3[redacted]');
}

/** A path without its query or fragment: what a failed request is logged as. */
export function pathOnly(url: string): string {
  try {
    const parsed = new URL(url, 'http://relative.invalid');
    // `protocol//host`, not `origin`: a non-special scheme (tauri://) has the origin "null".
    return parsed.host === 'relative.invalid' ? parsed.pathname : `${parsed.protocol}//${parsed.host}${parsed.pathname}`;
  } catch {
    return url.split(/[?#]/, 1)[0] ?? '';
  }
}

function describe(value: unknown): string {
  if (value instanceof Error) return value.stack !== undefined && value.stack !== '' ? `${value.name}: ${value.message}\n${value.stack}` : `${value.name}: ${value.message}`;
  if (typeof value === 'string') return value;
  try {
    return JSON.stringify(value) ?? String(value);
  } catch {
    return String(value);
  }
}

export interface HostLogger {
  log(level: LogLevel, message: string, location?: string): void;
}

let installed: HostLogger | undefined;

/** Log through the host when one is installed; otherwise nothing. */
export function hostLog(level: LogLevel, message: string, location?: string): void {
  installed?.log(level, message, location);
}

export function createHostLogger(invoke: HostInvoke): HostLogger {
  return {
    log(level, message, location) {
      const text = scrubLogText(message);
      const capped = text.length > MAX_MESSAGE_CHARS ? `${text.slice(0, MAX_MESSAGE_CHARS)}…[truncated]` : text;
      // A failed log write has nowhere to be reported (not console.error: that is forwarded here).
      void invoke('plugin:log|log', { level: LEVEL[level], message: capped, location: location ?? 'web' }).catch(() => undefined);
    },
  };
}

type ConsoleMethod = (...data: unknown[]) => void;

/**
 * Install the logger and forward the webview's errors to it. Returns an
 * uninstall (tests). Idempotent per logger: a second install replaces the first.
 */
export function installDiagnostics(logger: HostLogger, target: Window = window): () => void {
  installed = logger;
  const originalError: ConsoleMethod = console.error.bind(console);
  const originalWarn: ConsoleMethod = console.warn.bind(console);
  console.error = (...data: unknown[]) => {
    logger.log('error', data.map(describe).join(' '), 'console');
    originalError(...data);
  };
  console.warn = (...data: unknown[]) => {
    logger.log('warn', data.map(describe).join(' '), 'console');
    originalWarn(...data);
  };
  const onError = (event: ErrorEvent): void => {
    const where = event.filename === '' ? '' : ` at ${pathOnly(event.filename)}:${event.lineno}:${event.colno}`;
    logger.log('error', `${describe(event.error ?? event.message)}${where}`, 'window.error');
  };
  const onRejection = (event: PromiseRejectionEvent): void => {
    logger.log('error', describe(event.reason), 'unhandledrejection');
  };
  target.addEventListener('error', onError);
  target.addEventListener('unhandledrejection', onRejection);
  return () => {
    console.error = originalError;
    console.warn = originalWarn;
    target.removeEventListener('error', onError);
    target.removeEventListener('unhandledrejection', onRejection);
    if (installed === logger) installed = undefined;
  };
}

/** A request slower than this is logged even when it succeeds. */
const SLOW_REQUEST_MS = 2000;

/**
 * Wrap the native fetch so a failed or slow request is logged at debug:
 * method, path (never the query), status and time — or the network error.
 * Bodies and headers are never logged.
 */
export function withRequestLogging<F extends (input: string | URL | Request, init?: RequestInit) => Promise<Response>>(
  fetchImpl: F,
  now: () => number = () => performance.now(),
): F {
  const wrapped = async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    const method = (init?.method ?? (input instanceof Request ? input.method : 'GET')).toUpperCase();
    const started = now();
    const took = (): string => `${Math.round(now() - started)}ms`;
    try {
      const response = await fetchImpl(input, init);
      if (!response.ok || now() - started > SLOW_REQUEST_MS) {
        hostLog('debug', `${method} ${pathOnly(url)} -> ${response.status} in ${took()}`, 'http');
      }
      return response;
    } catch (error) {
      hostLog('debug', `${method} ${pathOnly(url)} -> failed in ${took()}: ${describe(error)}`, 'http');
      throw error;
    }
  };
  return wrapped as F;
}
