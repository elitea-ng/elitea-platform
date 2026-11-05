'use strict';

/**
 * Small retry helper with exponential backoff.
 * Used by the sync client and the webhook dispatcher.
 */

/** Thrown by retry() when every attempt has failed. */
class RetryError extends Error {
  constructor(message, attempts, lastError) {
    super(message);
    this.name = 'RetryError';
    this.attempts = attempts;
    this.lastError = lastError;
  }
}

const DEFAULTS = Object.freeze({
  maxAttempts: 5,
  baseDelayMs: 200,
  maxDelayMs: 10_000,
  factor: 2,
  jitter: true,
});

// Internal: "equal jitter" — a random delay between ms/2 and ms.
function jitter(ms) {
  return Math.floor(ms / 2 + Math.random() * (ms / 2));
}

// Internal: abortable sleep.
function sleep(ms, signal) {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason ?? new Error('Aborted'));
    const timer = setTimeout(resolve, ms);
    signal?.addEventListener('abort', () => {
      clearTimeout(timer);
      reject(signal.reason ?? new Error('Aborted'));
    }, { once: true });
  });
}

/**
 * Delay in milliseconds to wait after the given (1-based) failed attempt.
 * Grows as baseDelayMs * factor^(attempt-1), capped at maxDelayMs.
 */
function backoffDelay(attempt, options = {}) {
  if (!Number.isInteger(attempt) || attempt < 1) {
    throw new RangeError(`attempt must be an integer >= 1, got ${attempt}`);
  }
  const { baseDelayMs, maxDelayMs, factor, jitter: useJitter } = { ...DEFAULTS, ...options };
  const raw = Math.min(maxDelayMs, baseDelayMs * factor ** (attempt - 1));
  return useJitter ? jitter(raw) : raw;
}

/**
 * Default retry predicate.
 * - An explicit err.retryable boolean always wins.
 * - HTTP-like errors (numeric err.status): retry on 429 and 5xx only.
 * - Network errors: retry on ECONNRESET, ETIMEDOUT, EAI_AGAIN.
 */
function isRetryable(err) {
  if (err == null) return false;
  if (typeof err.retryable === 'boolean') return err.retryable;
  if (typeof err.status === 'number') return err.status === 429 || err.status >= 500;
  return ['ECONNRESET', 'ETIMEDOUT', 'EAI_AGAIN'].includes(err.code);
}

/**
 * Call fn until it succeeds, retrying failures that shouldRetry accepts.
 * fn receives the 1-based attempt number and may return a value or a promise.
 */
async function retry(fn, options = {}) {
  if (typeof fn !== 'function') {
    throw new TypeError('fn must be a function');
  }
  const opts = { ...DEFAULTS, shouldRetry: isRetryable, onRetry: undefined, signal: undefined, ...options };
  if (!Number.isInteger(opts.maxAttempts) || opts.maxAttempts < 1) {
    throw new RangeError('maxAttempts must be an integer >= 1');
  }

  let lastError;
  for (let attempt = 1; attempt <= opts.maxAttempts; attempt++) {
    if (opts.signal?.aborted) throw opts.signal.reason ?? new Error('Aborted');
    try {
      return await fn(attempt);
    } catch (err) {
      lastError = err;
      // Non-retryable errors are rethrown unchanged, not wrapped.
      if (!opts.shouldRetry(err, attempt)) throw err;
      if (attempt === opts.maxAttempts) break;
      const delay = backoffDelay(attempt, opts);
      opts.onRetry?.(err, attempt, delay);
      await sleep(delay, opts.signal);
    }
  }
  throw new RetryError(`gave up after ${opts.maxAttempts} attempts`, opts.maxAttempts, lastError);
}

module.exports = { retry, backoffDelay, isRetryable, RetryError };
