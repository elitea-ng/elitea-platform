/* eslint-disable no-console -- this module is the app's only console writer. */

/**
 * The app's only logger. Everything passes through `redact` first:
 *  - values under secret-looking or content keys (authorization, token, cookie,
 *    password, secret, content, user_input, …) are replaced;
 *  - bearer/JWT-looking strings are replaced wherever they appear;
 *  - query strings and fragments are stripped from URLs.
 * Debug output is dropped outside development builds.
 */

const SENSITIVE_KEY = /authorization|token|secret|password|passwd|cookie|session|pat$|^pat|api[-_]?key|content|user_input|message|body|prompt/i;
const BEARER = /\bbearer\s+[^\s"',]+/gi;
const JWT = /\beyJ[\w-]{6,}\.[\w-]{6,}\.[\w-]*/g;
const LONG_TOKEN = /\b[A-Za-z0-9_-]{32,}\b/g;
const URL_QUERY = /(https?:\/\/[^\s?#"']+)[?#][^\s"']*/gi;

export const REDACTED = '[redacted]';

function redactString(value: string): string {
  return value
    .replace(BEARER, `Bearer ${REDACTED}`)
    .replace(JWT, REDACTED)
    .replace(URL_QUERY, '$1')
    .replace(LONG_TOKEN, REDACTED);
}

export function redact(value: unknown, depth = 0): unknown {
  if (depth > 6) return '[depth]';
  if (typeof value === 'string') return redactString(value);
  if (value instanceof Error) {
    return { name: value.name, message: redactString(value.message) };
  }
  if (Array.isArray(value)) return value.map((item) => redact(item, depth + 1));
  if (value !== null && typeof value === 'object') {
    const out: Record<string, unknown> = {};
    for (const [key, inner] of Object.entries(value)) {
      out[key] = SENSITIVE_KEY.test(key) ? REDACTED : redact(inner, depth + 1);
    }
    return out;
  }
  return value;
}

type Level = 'debug' | 'info' | 'warn' | 'error';

function write(level: Level, message: string, fields?: Record<string, unknown>) {
  if (level === 'debug' && !__DEV__) return;
  const line = `[agent-zefir] ${redactString(message)}`;
  const args = fields === undefined ? [line] : [line, redact(fields)];
  switch (level) {
    case 'debug':
      console.debug(...args);
      break;
    case 'info':
      console.info(...args);
      break;
    case 'warn':
      console.warn(...args);
      break;
    case 'error':
      console.error(...args);
      break;
  }
}

export const log = {
  debug: (message: string, fields?: Record<string, unknown>) => write('debug', message, fields),
  info: (message: string, fields?: Record<string, unknown>) => write('info', message, fields),
  warn: (message: string, fields?: Record<string, unknown>) => write('warn', message, fields),
  error: (message: string, fields?: Record<string, unknown>) => write('error', message, fields),
};
