/**
 * lib/chatStreamToolOutputChunks.ts — the LIVE half of chunked tool output
 * (#956).
 *
 * A tool result larger than one output frame does not ride on the completed
 * call: it arrives ahead of it as an ordered sequence of
 * `agent_tool_output_chunk` events, each carrying a slice of the text, its
 * position, and the SHA-256 of the WHOLE output. The completed call then
 * carries `tool_output: ""` plus `tool_output_chunks {total, tool_output_sha256}`.
 * The contract is stated in full in
 * `services/elitea-main/internal/transport/runtimegrpc/nodeevent/tool_output_chunk.go`,
 * and elitea-main's trace projector reassembles the STORED row the same way.
 *
 * WHY THE BROWSER REASSEMBLES TOO. The stored row is only read back on a
 * reload. Without this, a user watching the turn live would see an empty tool
 * output for the whole call and then a full one after refreshing — and, worse,
 * the `agent_tool_end` arm accumulates `tool_output` by APPENDING, so the
 * empty string a chunked completion carries would leave whatever the pin had
 * (nothing) looking finished.
 *
 * WHAT IT REFUSES TO DO. It never presents an incomplete assembly as a whole
 * result. Chunks are collected BY INDEX (so an out-of-order delivery is
 * assembled correctly rather than concatenated in arrival order), and the
 * completion verifies the digest the producer stamped. A hole, or a digest
 * that does not match, marks the action `toolOutputPartial` and the pin says
 * so — a short output presented as complete is the failure the digest exists
 * to make impossible.
 */
import { sha256Hex } from '@/shared/lib/hash/sha256';

import { findToolAction, replaceAt, replaceToolAction } from './chatStreamShared';

import type { ChatMessage } from './convertMessagesToChatHistory';
import type { ToolAction } from './chatStreamToolAction';
import type { ChatStreamFrame } from './chatStreamFrame';

/** The event type one chunk rides on. */
const TOOL_OUTPUT_CHUNK_TYPE = 'agent_tool_output_chunk';

/** Where a partially assembled output lives on the action while it arrives. */
export const TOOL_OUTPUT_CHUNKS_KEY = 'toolOutputChunks';

/**
 * The assembly state of one chunked tool output, kept on the action itself so
 * the reducer stays pure: there is no module-level buffer to leak between
 * conversations or to survive a remount with stale text in it.
 */
export interface ToolOutputChunkState {
  /** Slices by index; a gap is a chunk that has not arrived (yet). */
  readonly parts: readonly (string | undefined)[];
  /** How many distinct indices have arrived. */
  readonly received: number;
  /** How many the producer says there are. */
  readonly total: number;
  /** SHA-256 of the whole output, as the producer computed it. */
  readonly digest: string;
  /** Set by the completion: every chunk present AND the digest matched. */
  readonly complete?: boolean | undefined;
  /** Set by the completion: it did not. */
  readonly partial?: boolean | undefined;
}

interface ChunkPosition {
  readonly toolCallId: string;
  readonly index: number;
  readonly total: number;
  readonly digest: string;
  readonly text: string;
}

function positiveInteger(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0 ? value : undefined;
}

/** Reads one chunk out of a frame, or `undefined` when the frame is not one. */
function chunkOf(frame: ChatStreamFrame): ChunkPosition | undefined {
  const raw = frame.response_metadata?.['tool_output_chunk'];
  if (typeof raw !== 'object' || raw === null) return undefined;
  const position = raw as Record<string, unknown>;
  const toolCallId = position['tool_call_id'];
  const digest = position['tool_output_sha256'];
  const index = positiveInteger(position['index']);
  const total = positiveInteger(position['total']);
  const text = frame.content;
  if (
    typeof toolCallId !== 'string' ||
    !toolCallId ||
    typeof digest !== 'string' ||
    !digest ||
    index === undefined ||
    total === undefined ||
    total === 0 ||
    index >= total ||
    typeof text !== 'string' ||
    !text
  ) {
    return undefined;
  }
  return { toolCallId, index, total, digest, text };
}

/** The action's current assembly state, if it has one for this digest. */
function stateOf(action: ToolAction, digest: string): ToolOutputChunkState | undefined {
  const raw = action[TOOL_OUTPUT_CHUNKS_KEY];
  if (typeof raw !== 'object' || raw === null) return undefined;
  const state = raw as ToolOutputChunkState;
  if (!Array.isArray(state.parts) || state.digest !== digest) return undefined;
  return state;
}

/**
 * The text a viewer may be shown right now: the assembled prefix, stopping at
 * the first gap.
 *
 * Stopping is the point. Joining everything that has arrived would splice
 * chunk 5 onto chunk 1 and show a document with its middle silently removed —
 * which reads exactly like a real result.
 */
function assembledPrefix(parts: readonly (string | undefined)[]): string {
  let text = '';
  for (const part of parts) {
    if (part === undefined) break;
    text += part;
  }
  return text;
}

/** Applies one chunk to the action's state. Later duplicates are no-ops. */
function withChunk(state: ToolOutputChunkState | undefined, chunk: ChunkPosition): ToolOutputChunkState {
  const parts: (string | undefined)[] = [
    ...(state?.parts ?? Array.from<string | undefined>({ length: chunk.total })),
  ];
  while (parts.length < chunk.total) parts.push(undefined);
  const alreadyHad = parts[chunk.index] !== undefined;
  parts[chunk.index] = chunk.text;
  return {
    parts,
    received: (state?.received ?? 0) + (alreadyHad ? 0 : 1),
    total: chunk.total,
    digest: chunk.digest,
  };
}

/**
 * Reduce one `agent_tool_output_chunk` frame, or return `undefined` for a
 * frame this family does not own so the dispatcher can offer it to the next.
 *
 * A chunk naming a tool call this message has no pin for is DROPPED rather
 * than failing the frame: a late subscriber can join a stream mid-tool, and
 * the completion's digest check is what notices the result is short.
 */
export function reduceToolOutputChunkFrame(
  history: readonly ChatMessage[],
  frame: ChatStreamFrame,
  type: string,
  index: number,
): readonly ChatMessage[] | undefined {
  if (type !== TOOL_OUTPUT_CHUNK_TYPE) return undefined;
  if (index === -1) return history;
  const current = history[index];
  const chunk = chunkOf(frame);
  if (!current || !chunk || !findToolAction(current, chunk.toolCallId)) return history;

  return replaceAt(history, index, {
    toolActions: replaceToolAction(current, chunk.toolCallId, (action) => {
      const next = withChunk(stateOf(action, chunk.digest), chunk);
      return {
        ...action,
        [TOOL_OUTPUT_CHUNKS_KEY]: next,
        // The pin shows the assembled prefix while the rest arrives, so a
        // large read is visibly streaming rather than blank until it lands.
        toolOutputs: assembledPrefix(next.parts),
      };
    }),
  });
}

/** What a completed call declares about an output that was chunked. */
export interface ChunkedCompletion {
  readonly total: number;
  readonly digest: string;
}

/**
 * Reads `tool_output_chunks` off a completed call, or `undefined` when the
 * result was small enough to ride inline (the overwhelming majority).
 */
export function chunkedCompletionOf(frame: ChatStreamFrame): ChunkedCompletion | undefined {
  const raw = frame.response_metadata?.['tool_output_chunks'];
  if (typeof raw !== 'object' || raw === null) return undefined;
  const declared = raw as Record<string, unknown>;
  const total = positiveInteger(declared['total']);
  const digest = declared['tool_output_sha256'];
  if (total === undefined || total === 0 || typeof digest !== 'string' || !digest) return undefined;
  return { total, digest };
}

/**
 * Finish one chunked output: verify what arrived against the producer's
 * digest, and say plainly which of the two outcomes this is.
 *
 * `partial` is not a detail. The alternative — quietly keeping the prefix —
 * hands the user a document that ends mid-sentence and looks finished, and
 * that is precisely the confusion the whole chunking contract is built to
 * avoid.
 */
export function finishChunkedToolOutput(
  action: ToolAction,
  completion: ChunkedCompletion,
): { readonly toolOutputs: string; readonly state: ToolOutputChunkState } {
  const state = stateOf(action, completion.digest);
  const parts = state?.parts ?? [];
  const whole = parts.length === completion.total && parts.every((part) => part !== undefined);
  const assembled = whole ? parts.join('') : assembledPrefix(parts);
  const verified = whole && sha256Hex(assembled) === completion.digest;
  return {
    toolOutputs: assembled,
    state: {
      parts,
      received: state?.received ?? 0,
      total: completion.total,
      digest: completion.digest,
      complete: verified,
      partial: !verified,
    },
  };
}
