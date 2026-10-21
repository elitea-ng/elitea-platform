import { z } from 'zod';

import type { SseEvent } from '@/api/sse/parser';

/**
 * Execution event-stream frames, validated at the boundary (the wire is not
 * specified by the server contract yet: ADR-0025 WP5). Schemas are loose: a
 * known field of the wrong type fails the frame, unknown fields pass through.
 */

export const EXECUTION_EVENT = {
  node: 'execution.node_event',
  failed: 'execution.failed',
  replayReset: 'execution.replay_reset',
} as const;

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
  /** Another event name (index ingest, configuration validation, future ones). */
  | { kind: 'other'; event: string }
  /** Data that is not valid JSON or does not match the schema. */
  | { kind: 'invalid'; event: string };

function parseJson(data: string): unknown {
  try {
    return JSON.parse(data);
  } catch {
    return undefined;
  }
}

export function parseFrame(event: SseEvent): Frame {
  switch (event.event) {
    case EXECUTION_EVENT.node: {
      const parsed = NodeEventSchema.safeParse(parseJson(event.data));
      return parsed.success ? { kind: 'node', node: parsed.data } : { kind: 'invalid', event: event.event };
    }
    case EXECUTION_EVENT.failed: {
      const parsed = ExecutionFailedSchema.safeParse(parseJson(event.data));
      return parsed.success ? { kind: 'failed', failure: parsed.data } : { kind: 'failed', failure: {} };
    }
    case EXECUTION_EVENT.replayReset: {
      const parsed = ReplayResetSchema.safeParse(parseJson(event.data));
      return { kind: 'replay_reset', reason: parsed.success ? (parsed.data.reason ?? null) : null };
    }
    default:
      return { kind: 'other', event: event.event };
  }
}
