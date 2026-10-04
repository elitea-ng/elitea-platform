/**
 * Public surface of the SSE transport module (issue #92). Named exports
 * only (`elitea/no-export-all`), curated the same way `shared/config`'s
 * barrel is.
 *
 * `./testing.ts` is deliberately NOT re-exported: it is test-only
 * machinery, imported deeply by the tests that need it — the same split
 * `shared/api/socket/testing.ts` already uses.
 */
export { useEventSource } from './useEventSource';
export type { ExecutionEventData } from './executionEvents';
/**
 * `useExecutionEventStream` subscribes by the `events_url` the START
 * endpoint returned, rather than re-deriving the path — the server owns that
 * shape. It was withheld from this barrel while the chat surface had no
 * consumer for the streamed envelope; `features/chat-messages`'s
 * `useChatStreamTransport` is that consumer, so the condition no longer
 * holds.
 */
export { useExecutionEventStream, useExecutionEvents } from './executionEvents';
/**
 * Resume + backoff policy for a dropped execution stream (issue #329). Kept
 * out of the hooks so the decisions are testable as values: how long to wait
 * before reopening, when to stop waiting, and how to ask the server for only
 * the frames this client missed.
 */
export { streamReconnectDelayMs, withResumeCursor } from './resume';
/**
 * Health of the always-on real-time channels, aggregated for the app shell's
 * connection indicator. Subscriptions report into it; the indicator reads it.
 */
export { createRealtimeStatusStore, RealtimeStatusContext, useRealtimeStatus, useReportRealtimeChannel } from './realtimeStatus';
export type { RealtimeChannelState, RealtimeStatus, RealtimeStatusStore } from './realtimeStatus';
