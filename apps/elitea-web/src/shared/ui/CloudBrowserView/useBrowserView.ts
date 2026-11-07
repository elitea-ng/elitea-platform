/**
 * One open cloud browser view, as React state: the stream of
 * `shared/api/browserView`, the latest frame, who drives, and whether this
 * viewer may. Reconnects on its own after a failure that may pass (busy, an
 * outage, a dropped connection, and "no browser yet": a run starts its
 * browser at its first browser call, so the view is retried slowly until it
 * does).
 */
import { useCallback, useEffect, useRef, useState } from 'react';

import {
  openBrowserView,
  type BrowserViewConnection,
  type BrowserViewFailure,
  type OpenBrowserViewOptions,
} from '@/shared/api/browserView/client';
import type { BrowserControl, BrowserViewFrameHeader } from '@/shared/api/browserView/protocol';

export type BrowserViewPhase = 'connecting' | 'live' | 'lingering' | 'ended' | 'failed';

export interface BrowserViewState {
  readonly phase: BrowserViewPhase;
  /** Main lets this viewer drive (and the session is not lingering). */
  readonly canControl: boolean;
  readonly control: BrowserControl;
  readonly url: string;
  readonly title: string;
  readonly width: number;
  readonly height: number;
  /** The reason the view is not open, while `phase` is `failed`. */
  readonly failure: BrowserViewFailure | null;
  /** How the session ended, while `phase` is `ended`. */
  readonly endCode: string | null;
  /** A short notice for the person: input refused, control revoked. */
  readonly notice: string | null;
}

const INITIAL: BrowserViewState = { phase: 'connecting', canControl: false, control: 'agent', url: '', title: '', width: 0, height: 0, failure: null, endCode: null, notice: null };

/** How long to wait before the next attempt: "not found" and "ended" wait for the run, the others pass soon. */
const RETRY_MS: Readonly<Record<BrowserViewFailure, number | null>> = {
  not_found: 15_000,
  busy: 5_000,
  unavailable: 5_000,
  disconnected: 2_000,
  ended: null,
  access_revoked: null,
  signed_out: null,
  forbidden: null,
};

export type FrameSink = (header: BrowserViewFrameHeader, jpeg: Blob) => Promise<void>;

export interface UseBrowserViewResult {
  readonly state: BrowserViewState;
  /** The open connection, for input; `null` while none is open. */
  readonly connection: BrowserViewConnection | null;
}

export function useBrowserView(sessionId: string | undefined, drawFrame: FrameSink, options: OpenBrowserViewOptions = {}): UseBrowserViewResult {
  const [state, setState] = useState<BrowserViewState>(INITIAL);
  const [connection, setConnection] = useState<BrowserViewConnection | null>(null);
  const [attempt, setAttempt] = useState(0);
  const draw = useRef(drawFrame);
  draw.current = drawFrame;
  const optionsRef = useRef(options);
  optionsRef.current = options;

  const patch = useCallback((next: Partial<BrowserViewState>) => setState((previous) => ({ ...previous, ...next })), []);

  useEffect(() => {
    if (sessionId === undefined) return undefined;
    let live = true;
    let retry: ReturnType<typeof setTimeout> | undefined;
    setState((previous) => ({ ...INITIAL, phase: previous.phase === 'failed' ? 'failed' : 'connecting', failure: previous.failure }));
    const view = openBrowserView(
      sessionId,
      {
        onHello: (hello) => {
          if (!live) return;
          patch({ phase: hello.lingering ? 'lingering' : 'live', canControl: hello.can_control && !hello.lingering, failure: null, notice: null });
          setConnection(view);
        },
        onFrame: (header, jpeg) => {
          if (!live) return;
          patch({ url: header.url, title: header.title, control: header.control, width: header.width, height: header.height });
          // Ack once the frame is on screen, also when it could not be drawn: the next one is the latest anyway.
          void draw.current(header, jpeg).catch(() => undefined).finally(() => {
            if (live) view.ack(header.seq);
          });
        },
        onNotice: (code) => {
          if (!live) return;
          if (code === 'control_revoked') patch({ canControl: false, notice: code });
          else if (code === 'session_lingering') patch({ phase: 'lingering', canControl: false });
          else patch({ notice: code });
        },
        onRefused: (code) => {
          if (live) patch({ notice: code });
        },
        onEnd: (code) => {
          if (!live) return;
          patch({ phase: 'ended', endCode: code, canControl: false });
          setConnection(null);
        },
        onFailure: (failure) => {
          if (!live) return;
          patch({ phase: 'failed', failure, canControl: false });
          setConnection(null);
          const wait = RETRY_MS[failure];
          if (wait !== null) retry = setTimeout(() => setAttempt((count) => count + 1), wait);
        },
      },
      optionsRef.current,
    );
    return () => {
      live = false;
      if (retry !== undefined) clearTimeout(retry);
      view.close();
      setConnection(null);
    };
  }, [sessionId, attempt, patch]);

  return { state, connection };
}
