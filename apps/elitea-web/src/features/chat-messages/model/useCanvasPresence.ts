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
 *
 * WHO "I" AM, AND WHY IT IS LOAD-BEARING. The roster the server answers a beat
 * with always contains the CALLER — the beat is what put it there. So the
 * read-only rule is only ever "is anyone here who is not me", and it is wrong
 * by exactly one entry for any client that cannot recognise its own. That was
 * the defect this file shipped with: the caller supplied no identity at all, a
 * single tab announced itself, read its own entry back as a stranger, and made
 * the editor read-only against the only person in it — CodeMirror rendered
 * `aria-readonly="true"` and the table grid disabled every cell. `userId` is
 * the identifier that fixes it (the server keys each entry by principal id and
 * puts it on the wire); the name is kept as the fallback for a roster with no
 * id, and an unidentifiable viewer stays editable rather than guessing.
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
  /**
   * The signed-in user's principal id — the SAME id the server puts on its own
   * roster entry (`Editor.user_id`).
   *
   * This is the identifier the self-check should use, and the name is only a
   * fallback for a roster that carries no id. The roster the server answers a
   * beat with ALWAYS contains the caller, so a viewer that cannot recognise
   * its own entry reads "one editor, none of them me" and locks the only
   * person editing out of their own canvas — see `isReadOnly` below.
   */
  readonly userId?: string | undefined;
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
  return editors.map((editor) => ({
    userName: editor.user_name,
    ...(typeof editor.user_id === 'string' && editor.user_id !== '' ? { userId: editor.user_id } : {}),
  }));
}

/**
 * Is this roster entry the viewer's own?
 *
 * The id wins when both the entry and the viewer carry one, because the id is
 * what the server keyed the entry by; the name is the fallback for a roster
 * that carries no id. An entry that matches NEITHER is somebody else.
 */
function isSelf(
  editor: CanvasEditorPresence,
  viewer: { readonly userId?: string | undefined; readonly userName?: string | undefined },
): boolean {
  if (viewer.userId !== undefined && viewer.userId !== '' && editor.userId !== undefined) {
    return editor.userId === viewer.userId;
  }
  if (viewer.userName !== undefined && viewer.userName !== '') return editor.userName === viewer.userName;
  return false;
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
  const { projectId, canvasId, userName, userId, state = 'editing', enabled = true } = params;

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
  /**
   * Can this client recognise its own roster entry at all? A viewer with no id
   * and no name cannot, and the read-only rule below then has no way to tell
   * "somebody else is here" from "I am here".
   */
  const knowsSelf = (userId !== undefined && userId !== '') || (userName !== undefined && userName !== '');
  const otherEditors = useMemo(
    () => (knowsSelf ? real.filter((editor) => !isSelf(editor, { userId, userName })) : real),
    [knowsSelf, real, userId, userName],
  );
  /*
   * Read-only exactly when somebody ELSE holds it. An EMPTY roster is editable,
   * which is what keeps behaviour unchanged with no second editor.
   *
   * FAIL OPEN when the viewer is unidentifiable. Every beat's own answer
   * contains the caller, so an unidentifiable viewer sees a roster of one
   * stranger and would go read-only on its own presence — a canvas nobody but
   * its single editor has open, locked against that editor, with no second tab
   * anywhere. This is not a lock in the first place (the server refuses no
   * write on this roster), so refusing to guess is strictly the safer half.
   */
  const isReadOnly = knowsSelf && real.length > 0 && otherEditors.length === real.length;

  return { editors: real, isReadOnly, otherEditors };
}
