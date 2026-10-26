import { z } from 'zod';

import {
  EXECUTION_EVENT,
  STEERING_EVENTS,
  STEERING_STATE,
  parseEventData,
  parseSteeringMessage,
  steeringDropReason,
  type SseEvent,
  type SteeringDropReason,
  type SteeringItem,
} from '@elitea/client-core/stream';

/**
 * Execution event-stream frames, validated at the boundary (the wire is not
 * specified by the server contract yet: ADR-0025 WP5). Schemas are loose: a
 * known field of the wrong type fails the frame, unknown fields pass through.
 * The event names and the frame vocabulary are shared with the web and
 * desktop clients (`@elitea/client-core/stream`).
 */

const ResponseMetadata = z.looseObject({
  finish_reason: z.string().nullish(),
  tool_name: z.string().nullish(),
  authorization_requests: z.array(z.unknown()).nullish(),
  hitl_interrupts: z.array(z.unknown()).nullish(),
  metadata: z.looseObject({}).nullish(),
});

export const NodeEventSchema = z.looseObject({
  type: z.string().min(1).max(128),
  content: z.unknown().optional(),
  thinking: z.string().nullish(),
  message_id: z.string().nullish(),
  question_id: z.string().nullish(),
  response_metadata: ResponseMetadata.nullish(),
});
export type NodeEvent = z.infer<typeof NodeEventSchema>;

export const ExecutionFailedSchema = z.looseObject({
  code: z.string().nullish(),
  safe_message: z.string().nullish(),
  retryable: z.boolean().nullish(),
});
export type ExecutionFailed = z.infer<typeof ExecutionFailedSchema>;

export const ReplayResetSchema = z.looseObject({ reason: z.string().nullish() });

export type Frame =
  | { kind: 'node'; node: NodeEvent }
  | { kind: 'failed'; failure: ExecutionFailed }
  | { kind: 'replay_reset'; reason: string | null }
  /**
   * A steering message of this turn (client contract 1.27): `steer.queued`,
   * `steer.injected` or `steer.dropped`. A dropped message has the state
   * `removed`, and `reason` is the drop reason (`removed` or `superseded`);
   * `reason` is null for the other events. A `superseded` message can name
   * an older answer than the turn of this stream.
   */
  | { kind: 'steer'; event: string; item: SteeringItem; reason: SteeringDropReason | null }
  /** Another event name (index ingest, configuration validation, future ones). */
  | { kind: 'other'; event: string }
  /** Data that is not valid JSON or does not match the schema. */
  | { kind: 'invalid'; event: string };

export function parseFrame(event: SseEvent): Frame {
  switch (event.event) {
    case EXECUTION_EVENT.node: {
      const parsed = NodeEventSchema.safeParse(parseEventData(event.data));
      return parsed.success ? { kind: 'node', node: parsed.data } : { kind: 'invalid', event: event.event };
    }
    case EXECUTION_EVENT.failed: {
      const parsed = ExecutionFailedSchema.safeParse(parseEventData(event.data));
      return parsed.success ? { kind: 'failed', failure: parsed.data } : { kind: 'failed', failure: {} };
    }
    case EXECUTION_EVENT.replayReset: {
      const parsed = ReplayResetSchema.safeParse(parseEventData(event.data));
      return { kind: 'replay_reset', reason: parsed.success ? (parsed.data.reason ?? null) : null };
    }
    default: {
      if (!STEERING_EVENTS.has(event.event)) return { kind: 'other', event: event.event };
      const data = parseEventData(event.data);
      const item = parseSteeringMessage(data);
      if (item === undefined) return { kind: 'invalid', event: event.event };
      const dropped = event.event === EXECUTION_EVENT.steerDropped;
      return {
        kind: 'steer',
        event: event.event,
        item: dropped ? { ...item, state: STEERING_STATE.Removed } : item,
        reason: dropped ? steeringDropReason(data) : null,
      };
    }
  }
}
