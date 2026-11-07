/**
 * The streamed browser pane (ADR-0048 decision 7, ADR-0049 decision 6): a
 * cloud run's browser drawn on a canvas, frame by frame, from the view
 * stream. One component for the web app's cloud threads (#1598) and the
 * desktop pane's streamed mode (#1592).
 *
 * - The toolbar shows the page's address (read-only: the agent drives the
 *   navigation, and a person who took over navigates inside the page) and
 *   the control pill: **Agent**, **You**, or **Ended**.
 * - **Take over** gives the person the page; the agent's browser calls then
 *   answer `user_in_control`. Clicking, typing or scrolling in the page while
 *   the agent drives takes it over too (ADR-0048 decision 4). **Hand back**
 *   returns it, with an optional note the agent reads.
 * - A viewer Main lets watch but not drive (and everyone while the session
 *   lingers after its run) sees "Watching only" and sends nothing but acks.
 *
 * Positions are scaled from the canvas box to the frame's logical pixels.
 */
import { useCallback, useEffect, useRef, useState, type KeyboardEvent, type PointerEvent, type WheelEvent } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import TextField from '@mui/material/TextField';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import type { BrowserViewConnection, OpenBrowserViewOptions } from '@/shared/api/browserView/client';
import { MAX_NOTE_BYTES, MODIFIER } from '@/shared/api/browserView/protocol';
import type { BrowserViewFrameHeader } from '@/shared/api/browserView/protocol';
import { t } from '@/shared/i18n';

import { describeEnd, describeFailure, describeNotice } from './describe';
import { useBrowserView, type BrowserViewState } from './useBrowserView';

export interface CloudBrowserViewProps {
  /** The run's browser session (`elitea-br-…`); nothing opens while it is `undefined`. */
  readonly sessionId: string | undefined;
  /** For tests: the stream's socket and the refusal check. */
  readonly streamOptions?: OpenBrowserViewOptions;
}

/** Draws one JPEG into the canvas, sized to the frame. */
async function paint(canvas: HTMLCanvasElement | null, header: BrowserViewFrameHeader, jpeg: Blob): Promise<void> {
  if (canvas === null) return;
  const context = canvas.getContext('2d');
  if (context === null) return;
  if (canvas.width !== header.width) canvas.width = header.width;
  if (canvas.height !== header.height) canvas.height = header.height;
  if (typeof createImageBitmap === 'function') {
    const bitmap = await createImageBitmap(jpeg);
    context.drawImage(bitmap, 0, 0, header.width, header.height);
    bitmap.close();
    return;
  }
  const url = URL.createObjectURL(jpeg);
  try {
    const image = new Image();
    image.src = url;
    await image.decode();
    context.drawImage(image, 0, 0, header.width, header.height);
  } finally {
    URL.revokeObjectURL(url);
  }
}

function modifiersOf(event: { altKey: boolean; ctrlKey: boolean; metaKey: boolean; shiftKey: boolean }): number {
  return (event.altKey ? MODIFIER.alt : 0) | (event.ctrlKey ? MODIFIER.ctrl : 0) | (event.metaKey ? MODIFIER.meta : 0) | (event.shiftKey ? MODIFIER.shift : 0);
}

/** DOM button (0 left, 1 middle, 2 right) to CDP's (1 left, 2 middle, 3 right). */
const BUTTON: Readonly<Record<number, number>> = { 0: 1, 1: 2, 2: 3 };

function Pill({ state }: { readonly state: BrowserViewState }): React.JSX.Element {
  const ended = state.phase === 'ended' || state.phase === 'failed';
  const label = ended
    ? t('cloudBrowser.pill.ended', 'Ended')
    : state.phase === 'connecting'
      ? t('cloudBrowser.pill.idle', 'Idle')
      : state.control === 'user'
        ? t('cloudBrowser.pill.you', 'You')
        : t('cloudBrowser.pill.agent', 'Agent');
  const color = !ended && state.control === 'user' && state.phase !== 'connecting' ? 'warning' : 'default';
  return <Chip size="small" label={label} color={color} data-testid="cloud-browser-pill" data-control={ended ? 'ended' : state.control} sx={{ flexShrink: 0 }} />;
}

function HandBack({ connection, onDone }: { readonly connection: BrowserViewConnection; readonly onDone: () => void }): React.JSX.Element {
  const [note, setNote] = useState('');
  const tooLong = new TextEncoder().encode(note).length > MAX_NOTE_BYTES;
  return (
    <Box component="form" data-testid="cloud-browser-hand-back-form" onSubmit={(event) => {
      event.preventDefault();
      if (tooLong) return;
      connection.handBack(note.trim());
      onDone();
    }} sx={(theme: Theme) => ({ display: 'flex', gap: 1, alignItems: 'flex-start', paddingX: 1, paddingY: 0.75, borderBottom: `1px solid ${theme.vars.palette.divider}` })}>
      <TextField
        size="small"
        fullWidth
        multiline
        maxRows={4}
        value={note}
        onChange={(event) => setNote(event.target.value)}
        placeholder={t('cloudBrowser.handBack.placeholder', 'A note for the agent, for example "signed in, continue"')}
        error={tooLong}
        helperText={tooLong ? t('cloudBrowser.handBack.tooLong', 'The note is too long.') : undefined}
        slotProps={{ htmlInput: { 'aria-label': t('cloudBrowser.handBack.note', 'Note for the agent'), 'data-testid': 'cloud-browser-note' } }}
      />
      <Button type="submit" size="small" variant="contained" disabled={tooLong} data-testid="cloud-browser-hand-back-send">
        {t('cloudBrowser.handBack', 'Hand back')}
      </Button>
      <Button size="small" onClick={onDone} data-testid="cloud-browser-hand-back-cancel">
        {t('cloudBrowser.cancel', 'Cancel')}
      </Button>
    </Box>
  );
}

function Toolbar({ state, connection, onHandBack }: { readonly state: BrowserViewState; readonly connection: BrowserViewConnection | null; readonly onHandBack: () => void }): React.JSX.Element {
  const driving = state.canControl && connection !== null;
  return (
    <Box data-testid="cloud-browser-toolbar" sx={(theme: Theme) => ({ display: 'flex', alignItems: 'center', gap: 0.75, paddingX: 1, paddingY: 0.5, borderBottom: `1px solid ${theme.vars.palette.divider}`, flexShrink: 0 })}>
      <Tooltip title={state.title}>
        <Typography variant="bodySmall" data-testid="cloud-browser-address" aria-label={t('cloudBrowser.address', 'Address')} noWrap sx={(theme: Theme) => ({ flex: 1, minWidth: 0, paddingX: 1, height: '1.75rem', lineHeight: '1.75rem', borderRadius: 'var(--el-shape-radiusSm, 4px)', background: theme.vars.palette.background.default })}>
          {state.url}
        </Typography>
      </Tooltip>
      <Pill state={state} />
      {driving && state.control === 'agent' && (
        <Button size="small" variant="outlined" onClick={() => connection.takeOver()} data-testid="cloud-browser-take-over">
          {t('cloudBrowser.takeOver', 'Take over')}
        </Button>
      )}
      {driving && state.control === 'user' && (
        <Button size="small" variant="contained" onClick={onHandBack} data-testid="cloud-browser-hand-back">
          {t('cloudBrowser.handBack', 'Hand back')}
        </Button>
      )}
    </Box>
  );
}

function Notice({ children, testId }: { readonly children: React.ReactNode; readonly testId: string }): React.JSX.Element {
  return (
    <Typography variant="bodySmall" component="output" data-testid={testId} sx={(theme: Theme) => ({ display: 'block', paddingX: 1.5, paddingY: 0.75, color: theme.vars.palette.text.metrics, borderBottom: `1px solid ${theme.vars.palette.divider}`, flexShrink: 0 })}>
      {children}
    </Typography>
  );
}

function statusLine(state: BrowserViewState): string | null {
  if (state.phase === 'failed' && state.failure !== null) return describeFailure(state.failure);
  if (state.phase === 'ended') return describeEnd(state.endCode);
  if (state.phase === 'lingering') return t('cloudBrowser.lingering', 'The run ended. The last page stays here for a few minutes; nobody can take over.');
  if (state.phase === 'live' && !state.canControl) return t('cloudBrowser.watchOnly', 'Watching only. You can see the browser but not use it.');
  return null;
}

function useInput(connection: BrowserViewConnection | null, state: BrowserViewState, canvas: React.RefObject<HTMLCanvasElement | null>) {
  const enabled = connection !== null && state.canControl;
  const at = useCallback((event: { clientX: number; clientY: number }): [number, number] => {
    const box = canvas.current?.getBoundingClientRect();
    if (box === undefined || box.width === 0 || box.height === 0) return [0, 0];
    return [((event.clientX - box.left) / box.width) * state.width, ((event.clientY - box.top) / box.height) * state.height];
  }, [canvas, state.width, state.height]);
  const lastMove = useRef(0);
  if (!enabled) return {};
  return {
    onPointerDown: (event: PointerEvent<HTMLCanvasElement>) => {
      event.currentTarget.focus();
      const [x, y] = at(event);
      connection.pointer('down', x, y, BUTTON[event.button] ?? 0, Math.min(event.detail || 1, 3), modifiersOf(event));
    },
    onPointerUp: (event: PointerEvent<HTMLCanvasElement>) => {
      const [x, y] = at(event);
      connection.pointer('up', x, y, BUTTON[event.button] ?? 0, Math.min(event.detail || 1, 3), modifiersOf(event));
    },
    onPointerMove: (event: PointerEvent<HTMLCanvasElement>) => {
      // A move pauses the agent only while the person holds a button; a hover is sent at most 20 times a second.
      const now = Date.now();
      if (event.buttons === 0 && (state.control !== 'user' || now - lastMove.current < 50)) return;
      lastMove.current = now;
      const [x, y] = at(event);
      connection.pointer('move', x, y, BUTTON[event.button] ?? 0, 0, modifiersOf(event));
    },
    onWheel: (event: WheelEvent<HTMLCanvasElement>) => {
      const [x, y] = at(event);
      connection.wheel(x, y, event.deltaX, event.deltaY, modifiersOf(event));
    },
    onKeyDown: (event: KeyboardEvent<HTMLCanvasElement>) => {
      event.preventDefault();
      const modifiers = modifiersOf(event);
      connection.key('down', event.key, event.code, '', modifiers);
      if (event.key.length === 1 && !event.ctrlKey && !event.metaKey) connection.key('char', event.key, event.code, event.key, modifiers);
    },
    onKeyUp: (event: KeyboardEvent<HTMLCanvasElement>) => {
      event.preventDefault();
      connection.key('up', event.key, event.code, '', modifiersOf(event));
    },
    onContextMenu: (event: React.MouseEvent) => event.preventDefault(),
  };
}

type CanvasInput = ReturnType<typeof useInput>;

/** The page: a focusable canvas when the viewer may drive, a picture otherwise. */
function PageCanvas({ canvas, title, input, shown }: { readonly canvas: React.RefObject<HTMLCanvasElement | null>; readonly title: string; readonly input: CanvasInput; readonly shown: boolean }): React.JSX.Element {
  const driving = input.onPointerDown !== undefined;
  return (
    <Box
      component="canvas"
      ref={canvas}
      tabIndex={driving ? 0 : -1}
      aria-label={title === '' ? t('cloudBrowser.page', 'The cloud browser page') : title}
      data-testid="cloud-browser-canvas"
      {...input}
      sx={{ display: shown ? 'block' : 'none', maxWidth: '100%', maxHeight: '100%', objectFit: 'contain', cursor: driving ? 'pointer' : 'default', outline: 'none', touchAction: 'none' }}
    />
  );
}

export function CloudBrowserView({ sessionId, streamOptions }: CloudBrowserViewProps): React.JSX.Element {
  const canvas = useRef<HTMLCanvasElement | null>(null);
  const [handingBack, setHandingBack] = useState(false);
  const drawFrame = useCallback((header: BrowserViewFrameHeader, jpeg: Blob) => paint(canvas.current, header, jpeg), []);
  const { state, connection } = useBrowserView(sessionId, drawFrame, streamOptions);
  const input = useInput(connection, state, canvas);
  const status = statusLine(state);
  const notice = describeNotice(state.notice);
  // The person handed back, or lost control: the note form goes.
  useEffect(() => {
    if (state.control !== 'user' || !state.canControl) setHandingBack(false);
  }, [state.control, state.canControl]);
  const shown = state.width > 0 && state.phase !== 'failed';

  return (
    <Box data-testid="cloud-browser" data-phase={state.phase} sx={{ flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column' }}>
      <Toolbar state={state} connection={connection} onHandBack={() => setHandingBack(true)} />
      {handingBack && connection !== null && <HandBack connection={connection} onDone={() => setHandingBack(false)} />}
      {status !== null && <Notice testId="cloud-browser-status">{status}</Notice>}
      {notice !== null && <Notice testId="cloud-browser-notice">{notice}</Notice>}
      <Box sx={{ flex: 1, minHeight: 0, position: 'relative', display: 'flex', alignItems: 'flex-start', justifyContent: 'center', overflow: 'hidden' }}>
        <PageCanvas canvas={canvas} title={state.title} input={input} shown={shown} />
        {!shown && state.phase === 'connecting' && (
          <Typography variant="bodySmall" data-testid="cloud-browser-empty" sx={(theme: Theme) => ({ padding: 3, textAlign: 'center', color: theme.vars.palette.text.metrics })}>
            {t('cloudBrowser.connecting', 'Connecting to the browser…')}
          </Typography>
        )}
      </Box>
    </Box>
  );
}

