/**
 * One background process's terminal (#1223), on xterm.js: what the host still
 * holds of its output (`process_read`), then its live `process_output`, the
 * person's keystrokes back to it (`process_write`) and, for a terminal
 * process, the pane's size (`process_resize`). Loaded only with the
 * Processes tab (`TerminalPane`, lazy).
 */
import { useEffect, useRef } from 'react';

import { FitAddon } from '@xterm/addon-fit';
import { Terminal } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';

import type { ProcessInfo, ProcessIpc } from '@/shared/desktop/processIpc';
import type { ProcessOutputPayload } from '@/shared/desktop/processTypes';

const encoder = new TextEncoder();

interface TerminalViewProps {
  ipc: ProcessIpc;
  workspaceId: string;
  process: ProcessInfo;
}

/** Fit the terminal to its box and tell the host a terminal process's new size. */
function fitter(fit: FitAddon, term: Terminal, resize: (rows: number, cols: number) => void): () => void {
  return () => {
    try {
      fit.fit();
    } catch {
      return; // not laid out yet (a hidden tab)
    }
    resize(term.rows, term.cols);
  };
}

export function TerminalView({ ipc, workspaceId, process }: TerminalViewProps): React.JSX.Element {
  const host = useRef<HTMLDivElement>(null);
  const terminal = useRef<Terminal | null>(null);
  const { process_id: processId, tty } = process;
  const running = process.status === 'running';

  useEffect(() => {
    const element = host.current;
    if (element === null) return undefined;
    const style = getComputedStyle(element);
    const term = new Terminal({
      convertEol: !tty,
      fontFamily: style.fontFamily,
      fontSize: 12,
      scrollback: 5000,
      theme: { background: style.backgroundColor, foreground: style.color },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(element);
    terminal.current = term;
    // Bytes written so far; live output arriving before the catch-up read answers waits for it.
    let end = 0;
    let caughtUp = false;
    let disposed = false;
    let unlisten: () => void = () => undefined;
    const pending: ProcessOutputPayload[] = [];
    const take = (payload: ProcessOutputPayload): void => {
      const after = payload.offset + encoder.encode(payload.data).length;
      if (after <= end) return;
      term.write(payload.data);
      end = after;
    };
    const settle = (): void => {
      caughtUp = true;
      pending.splice(0).forEach(take);
    };
    void ipc
      .onEvent((event) => {
        if (event.workspace_id !== workspaceId || event.kind !== 'process_output' || event.payload.process_id !== processId) return;
        if (caughtUp) take(event.payload);
        else pending.push(event.payload);
      })
      .then((off) => {
        if (disposed) off();
        else unlisten = off;
        return ipc.read(workspaceId, processId);
      })
      .then(
        (output) => {
          if (disposed) return;
          term.write(output.data);
          end = output.next_offset;
          settle();
        },
        settle,
      );
    const input = term.onData((data) => void ipc.write(workspaceId, processId, data).catch(() => undefined));
    const refit = fitter(fit, term, (rows, cols) => {
      if (tty) void ipc.resize(workspaceId, processId, rows, cols).catch(() => undefined);
    });
    const observer = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(refit);
    observer?.observe(element);
    refit();
    return () => {
      disposed = true;
      observer?.disconnect();
      input.dispose();
      unlisten();
      terminal.current = null;
      term.dispose();
    };
  }, [ipc, workspaceId, processId, tty]);

  useEffect(() => {
    if (terminal.current !== null) terminal.current.options.disableStdin = !running;
  }, [running]);

  return (
    <Box
      ref={host}
      data-testid="terminal-view"
      sx={(theme: Theme) => ({
        flex: 1,
        minHeight: '16rem',
        padding: 0.5,
        borderRadius: theme.vars.shape.radiusSm,
        background: theme.vars.palette.background.default,
        color: theme.vars.palette.text.primary,
        fontFamily: theme.typography.fontFamilyMono,
      })}
    />
  );
}
