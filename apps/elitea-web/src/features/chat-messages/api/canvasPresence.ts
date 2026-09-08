/**
 * api/canvasPresence.ts — the canvas presence heartbeat (#622).
 *
 *   POST /elitea_core/canvas/prompt_lib/{projectId}/{canvasId}/presence
 *
 * Hand-registered per R-A5 (`eliteaFetch` plus the matching
 * `endpoints.manifest.json` row). orval DOES generate a hook for this
 * operation, and it is deliberately not used: orval shapes every write
 * endpoint as a `useQuery` gated by `enabled`, and this is a beat sent from an
 * effect and from a `visibilitychange` listener, neither of which is a render.
 *
 * WHY THERE IS NO ROOM NAME IN THIS FILE. The event this beat triggers is
 * published on the project's SSE channel, whose name the server derives from
 * the `{projectId}` of the gated mount pattern. The client never names a room —
 * that is the whole difference between this and the socket.io prototype #615
 * declined to rebuild, whose room was `"canvas:" + canvasId` with no project
 * component.
 */
import { eliteaFetch } from '@/shared/api/generated/mutator';

/** One person with the canvas open. The server's field names are the reference's. */
export interface CanvasPresenceEditorWire {
  readonly user_id: string;
  readonly user_name: string;
  readonly user_avatar?: string;
  readonly state?: 'editing' | 'viewing';
}

/**
 * The roster. The POST answers this AND the `canvas.editors` SSE frame carries
 * the same shape, so one parser serves both.
 */
export interface CanvasPresenceWire {
  readonly project_id: string;
  readonly canvas_uuid: string;
  readonly message_group_uuid: string;
  readonly editors: readonly CanvasPresenceEditorWire[];
  readonly ttl_seconds: number;
}

/** The states a beat may declare. `left` removes the caller from the roster now. */
export type CanvasPresenceState = 'editing' | 'viewing' | 'left';

/**
 * Send one beat and read the roster back.
 *
 * `keepalive` is set so the `left` beat sent from an unmount or a page hide
 * still reaches the server after the document is gone. Without it a closing tab
 * cancels its own in-flight request and the entry lingers for a full TTL —
 * which is exactly the reference's behaviour, and the thing this fixes.
 */
export async function sendCanvasPresence(
  projectId: string | number,
  canvasId: string,
  state: CanvasPresenceState,
  options: { readonly signal?: AbortSignal; readonly keepalive?: boolean } = {},
): Promise<CanvasPresenceWire> {
  // Every hand-registered endpoint here unwraps orval's {data,status,headers}
  // envelope; reading it as the body makes every field `undefined` (#132).
  const envelope = await eliteaFetch<{ data: CanvasPresenceWire }>(
    `/elitea_core/canvas/prompt_lib/${projectId}/${canvasId}/presence`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ state }),
      ...(options.signal ? { signal: options.signal } : {}),
      ...(options.keepalive === true ? { keepalive: true } : {}),
    },
  );
  return envelope.data;
}
