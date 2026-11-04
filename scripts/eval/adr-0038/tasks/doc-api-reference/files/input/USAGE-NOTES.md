# Notes from the team chat about retry.js

- "The sync client calls `retry(() => client.push(batch), { maxAttempts: 8 })`."
- "Webhook dispatcher passes `onRetry` to log each retry with the delay."
- "Remember: a 404 is never retried by default; only 429 and 5xx."
- "If you pass an AbortSignal and abort it, retry() rejects with the signal's
  reason instead of a RetryError."
- "jitter() and sleep() are internal, please don't import them; they may
  change without notice."
