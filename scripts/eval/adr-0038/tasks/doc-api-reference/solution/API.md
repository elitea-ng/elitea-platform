# API reference: `retry.js`

A small retry helper with exponential backoff. Exports: `retry`,
`backoffDelay`, `isRetryable` and `RetryError`.

```js
const { retry, backoffDelay, isRetryable, RetryError } = require('./retry');
```

## retry(fn, options)

Calls `fn` until it succeeds, retrying failures that `shouldRetry` accepts and
waiting `backoffDelay(attempt, options)` between attempts.

**Parameters**

- `fn` (`(attempt: number) => T | Promise<T>`, required): the operation. It
  receives the 1-based attempt number.
- `options` (`object`, default `{}`):
  - `maxAttempts` (`number`, default `5`): total attempts, including the first.
  - `baseDelayMs` (`number`, default `200`): delay after the first failure.
  - `maxDelayMs` (`number`, default `10000`): upper bound for any delay.
  - `factor` (`number`, default `2`): growth factor per attempt.
  - `jitter` (`boolean`, default `true`): randomise each delay to between half
    and all of the computed value.
  - `shouldRetry` (`(err, attempt) => boolean`, default `isRetryable`): decides
    whether a failure is retried.
  - `onRetry` (`(err, attempt, delayMs) => void`, default `undefined`): called
    before each wait.
  - `signal` (`AbortSignal`, default `undefined`): aborts the loop and any
    pending wait.

**Returns** `Promise<T>`: resolves with the first successful result of `fn`.

**Throws / rejects**

- `TypeError` if `fn` is not a function.
- `RangeError` if `maxAttempts` is not an integer >= 1.
- The original error, unchanged, if `shouldRetry` returns `false` for it.
- `RetryError` when all `maxAttempts` attempts failed with retryable errors.
- The signal's `reason` (or `Error('Aborted')`) if `signal` is aborted.

```js
const result = await retry(() => client.push(batch), {
  maxAttempts: 8,
  onRetry: (err, attempt, delay) =>
    console.warn(`attempt ${attempt} failed, retrying in ${delay} ms`, err),
});
```

## backoffDelay(attempt, options)

Returns the delay to wait after a failed attempt:
`min(maxDelayMs, baseDelayMs * factor ** (attempt - 1))`, with jitter applied
if enabled.

**Parameters**

- `attempt` (`number`, required): the 1-based number of the failed attempt.
- `options` (`object`, default `{}`):
  - `baseDelayMs` (`number`, default `200`)
  - `maxDelayMs` (`number`, default `10000`)
  - `factor` (`number`, default `2`)
  - `jitter` (`boolean`, default `true`)

**Returns** `number`: the delay in milliseconds. With `jitter: true` it is a
random whole number between half the computed delay and the computed delay.

**Throws** `RangeError` if `attempt` is not an integer >= 1.

```js
backoffDelay(1, { jitter: false }); // 200
backoffDelay(3, { jitter: false }); // 800
backoffDelay(10, { jitter: false }); // 10000 (capped by maxDelayMs)
```

## isRetryable(err)

The default `shouldRetry` predicate.

**Parameters**

- `err` (`any`): the error thrown by `fn`.

**Returns** `boolean`:

- `false` for `null` or `undefined`;
- `err.retryable` if it is a boolean (an explicit flag always wins);
- for errors with a numeric `err.status`: `true` only for `429` and `5xx`
  (so a `404` is never retried);
- otherwise `true` if `err.code` is `ECONNRESET`, `ETIMEDOUT` or `EAI_AGAIN`.

**Throws** nothing.

```js
isRetryable({ status: 503 });                    // true
isRetryable({ status: 404 });                    // false
isRetryable({ code: 'ECONNRESET' });             // true
isRetryable({ status: 500, retryable: false });  // false
```

## RetryError

`class RetryError extends Error`, thrown (as a rejection) by `retry` when
every attempt failed with a retryable error.

**Properties**

- `name` (`string`): always `'RetryError'`.
- `message` (`string`): e.g. `gave up after 5 attempts`.
- `attempts` (`number`): the number of attempts made (`maxAttempts`).
- `lastError` (`any`): the error thrown by the last attempt.

```js
try {
  await retry(fetchReport);
} catch (err) {
  if (err instanceof RetryError) {
    console.error(`failed ${err.attempts} times`, err.lastError);
  } else {
    throw err;
  }
}
```
