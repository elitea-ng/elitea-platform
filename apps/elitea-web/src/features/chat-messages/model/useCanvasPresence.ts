/**
 * model/useCanvasPresence.ts — canvas editor presence over the project SSE
 * plane (#622).
 *
 * This is the client half of the decision #615 recorded: DO NOT rebuild the
 * socket.io server. The transport is
 *
 *   POST /elitea_core/canvas/prompt_lib/{projectId}/{canvasId}/presence  (beat)
 *   GET  /elitea_core/events/prompt_lib/{projectId}                      (fan-out)
 *
 * and the second of those is the project stream the app already opens for
 * notifications-shaped traffic — no new room, no new connection contract.
 *
 * THE BEAT SCHEDULE, and why each part exists:
 *   - on mount / when the canvas changes — announce, and learn who is already
 *     there. The POST's own answer is the roster, so a single tab is correct
 *     before any SSE frame arrives.
 *   - on a timer at a THIRD of the server's `ttl_seconds` — two consecutive
 *     lost beats still do not evict a live tab. The interval comes from the
 *     server's answer rather than from a constant here, so the two halves
 *     cannot drift.
 *   - on `visibilitychange` — hidden sends `left`, visible re-announces. A
 *     backgrounded tab is not editing, and browsers throttle its timers anyway,
 *     so without this it would look present until its TTL and then flicker.
 *   - on unmount — `left`, with `keepalive`, so a closing tab is removed at once
 *     rather than lingering for a TTL.
 *
 * WHAT IT DOES NOT DO. It does not lock. `isCanvasReadOnlyForUser` is what the
 * caller uses to go read-only, which is what the reference user saw, but the
 * server refuses no write on the roster: a lock enforced by the client alone is
 * a control that looks live and never fires.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import type { CanvasEditorPresence } from '@/entities/canvas';
import { realCanvasEditors } from '@/entities/canvas';
import { useEventSource } from '@/shared/api/sse';
import { getConfig } from '@/shared/config';

import type { CanvasPresenceEditorWire, CanvasPresenceState, CanvasPresenceWire } from '../api/canvasPresence';
import { sendCanvasPresence } from '../api/canvasPresence';

/**
 * The SSE `event:` name the server writes. It is the DomainEvent type
 * (`internal/api/v2/canvaspresence`'s EventType), not the reference's socket
 * spelling `chat_canvas_editors_change` — the transport is not the socket.
 */
const PRESENCE_EVENT = 'canvas.editors';

/**
 * Used only if a server answer somehow carries no `ttl_seconds`. It matches the
 * server's own default (120s), and the resend divisor below turns it into a
 * 40-second beat.
 */
const FALLBACK_TTL_SECONDS = 120;

/** Beats per TTL. Three means two may be lost without evicting a live tab. */
const BEATS_PER_TTL = 3;

export interface UseCanvasPresenceParams {
  /** The project the canvas lives in. Absent means "not resolved yet" — the hook idles. */
  readonly projectId?: string | number | undefined;
  /** The canvas, by uuid or row id. Absent means this block is not a shared canvas. */
  readonly canvasId?: string | undefined;
  /** The signed-in user's display name, for the read-only decision. */
  readonly userName?: string | undefined;
  /** What to announce while the tab is visible. */
  readonly state?: 'editing' | 'viewing';
  /** Set false to keep the hook mounted and silent (e.g. a read-only preview). */
  readonly enabled?: boolean;
}

export interface UseCanvasPresenceResult {
  /** The roster, with the two service principals removed. Never undefined. */
  readonly editors: readonly CanvasEditorPresence[];
  /** True while somebody ELSE holds the canvas. False when the roster is empty. */
  readonly isReadOnly: boolean;
  /** The other people on this canvas — the roster minus the signed-in user. */
  readonly otherEditors: readonly CanvasEditorPresence[];
}

/** The wire roster, mapped onto the entity shape the selectors already speak. */
function toPresence(editors: readonly CanvasPresenceEditorWire[] | undefined): CanvasEditorPresence[] {
  if (!editors) return [];
  return editors.map((editor) => ({ userName: editor.user_name }));
}

/**
 * Parse one SSE frame. The frame's `data` is the server's Response encoded as
 * JSON; anything else is dropped rather than thrown, because a stream is not a
 * place to crash the editor from (§3.6).
 */
function parseFrame(raw: string): CanvasPresenceWire | null {
  try {
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== 'object' || parsed === null) return null;
    const candidate = parsed as Partial<CanvasPresenceWire>;
    if (typeof candidate.canvas_uuid !== 'string' || !Array.isArray(candidate.editors)) return null;
    return candidate as CanvasPresenceWire;
  } catch {
    return null;
  }
}

export function useCanvasPresence(params: UseCanvasPresenceParams): UseCanvasPresenceResult {
  const { projectId, canvasId, userName, state = 'editing', enabled = true } = params;

  const [editors, setEditors] = useState<readonly CanvasEditorPresence[]>([]);
  const [ttlSeconds, setTtlSeconds] = useState(FALLBACK_TTL_SECONDS);
  /**
   * The uuid the SERVER resolved. The `canvasId` prop may be a row id, and the
   * SSE frames are keyed by uuid, so a frame can only be matched after the first
   * answer comes back. Until then no frame is applied — which is correct: the
   * POST's own answer is more recent than any frame that predates it.
   */
  const resolvedUuidRef = useRef<string | null>(null);

  const active = enabled && projectId !== undefined && projectId !== null && projectId !== '' && !!canvasId;

  const stateRef = useRef(state);
  stateRef.current = state;

  const applyRoster = useCallback((roster: CanvasPresenceWire) => {
    resolvedUuidRef.current = roster.canvas_uuid;
    setEditors(toPresence(roster.editors));
    if (typeof roster.ttl_seconds === 'number' && roster.ttl_seconds > 0) {
      setTtlSeconds(roster.ttl_seconds);
    }
  }, []);

  const beat = useCallback(
    (beatState: CanvasPresenceState, keepalive = false) => {
      if (!active || projectId === undefined || projectId === null || !canvasId) return;
      void sendCanvasPresence(projectId, canvasId, beatState, { keepalive })
        .then((roster) => {
          // A `left` beat's answer is the roster WITHOUT this tab, which is the
          // right thing to render if the component is somehow still mounted.
          applyRoster(roster);
        })
        .catch(() => {
          // A failed beat is not an error the editor can act on: the entry
          // expires on the server's TTL and the next beat re-announces. What it
          // must NOT do is leave a stale roster claiming a lock, so the local
          // view is cleared instead.
          setEditors([]);
        });
    },
    [active, applyRoster, canvasId, projectId],
  );

  // Announce on mount and whenever the canvas or project changes; leave on the
  // way out. `beat` is stable for a given (project, canvas), so this effect does
  // not re-run on every render.
  useEffect(() => {
    if (!active) {
      setEditors([]);
      resolvedUuidRef.current = null;
      return undefined;
    }
    beat(stateRef.current);
    return () => {
      beat('left', true);
      resolvedUuidRef.current = null;
    };
  }, [active, beat]);

  // The resend timer, driven by the server's own TTL.
  useEffect(() => {
    if (!active) return undefined;
    const intervalMs = Math.max(1000, Math.floor((ttlSeconds * 1000) / BEATS_PER_TTL));
    const timer = setInterval(() => {
      if (typeof document !== 'undefined' && document.visibilityState === 'hidden') return;
      beat(stateRef.current);
    }, intervalMs);
    return () => clearInterval(timer);
  }, [active, beat, ttlSeconds]);

  // A hidden tab is not editing. Announcing again on the way back is what makes
  // a re-shown tab visible to the others without waiting for the next tick.
  useEffect(() => {
    if (!active || typeof document === 'undefined') return undefined;
    const onVisibilityChange = (): void => {
      if (document.visibilityState === 'hidden') {
        beat('left', true);
      } else {
        beat(stateRef.current);
      }
    };
    document.addEventListener('visibilitychange', onVisibilityChange);
    return () => document.removeEventListener('visibilitychange', onVisibilityChange);
  }, [active, beat]);

  // The fan-out. Every other tab's beat arrives here.
  const config = getConfig();
  const serverUrl = config.status === 'ok' ? config.config.vite_server_url : null;
  const streamUrl = useMemo(
    () => (active && serverUrl ? `${serverUrl}/elitea_core/events/prompt_lib/${String(projectId)}` : null),
    [active, projectId, serverUrl],
  );

  useEventSource(streamUrl, {
    [PRESENCE_EVENT]: (event: MessageEvent) => {
      const roster = parseFrame(String(event.data));
      if (!roster) return;
      // The project channel carries EVERY canvas in the project. A frame for a
      // different canvas is not ours.
      if (resolvedUuidRef.current !== null && roster.canvas_uuid !== resolvedUuidRef.current) return;
      applyRoster(roster);
    },
  });

  const real = useMemo(() => realCanvasEditors(editors), [editors]);
  const otherEditors = useMemo(
    () => (userName ? real.filter((editor) => editor.userName !== userName) : real),
    [real, userName],
  );
  // Read-only exactly when somebody else holds it. An EMPTY roster is editable,
  // which is what keeps behaviour unchanged with no second editor.
  const isReadOnly = real.length > 0 && otherEditors.length === real.length;

  return { editors: real, isReadOnly, otherEditors };
}
