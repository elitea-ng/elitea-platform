/**
 * The session's Processes tab (#1223; IPC.md "Background processes"): the
 * workspace's background processes, newest first, with their state, Pin /
 * Unpin (a pinned process outlives its turn) and Stop, and the chosen one's
 * live terminal (`TerminalView`), which the person can type into. Loaded
 * lazily, once the workspace has a process (`plan.TerminalPane`).
 */
import { useEffect, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import ButtonBase from '@mui/material/ButtonBase';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { tauriProcessIpc, type ProcessInfo, type ProcessIpc } from '@/shared/desktop/processIpc';
import { t } from '@/shared/i18n';

import { TerminalView } from './TerminalView';

export interface TerminalPaneProps {
  workspaceId: string;
  /** The host client; the Tauri one when absent (tests pass a fake). */
  ipc?: ProcessIpc;
}

function statusLabel(process: ProcessInfo): string {
  switch (process.status) {
    case 'running':
      return process.pinned ? t('workspace.processes.pinned', 'Running, pinned') : t('workspace.processes.running', 'Running');
    case 'exited':
      return t('workspace.processes.exited', 'Exited ({{code}})', { code: String(process.exit_code ?? '?') });
    case 'killed':
      return t('workspace.processes.killed', 'Stopped');
    case 'timed_out':
      return t('workspace.processes.timedOut', 'Timed out');
  }
}

/** `list` with `next` in place of the process of the same id (or added). */
function upsert(list: readonly ProcessInfo[], next: ProcessInfo): ProcessInfo[] {
  return list.some((process) => process.process_id === next.process_id)
    ? list.map((process) => (process.process_id === next.process_id ? next : process))
    : [...list, next];
}

/** The workspace's processes: listed once, then kept by `process://event`. */
function useProcesses(ipc: ProcessIpc | undefined, workspaceId: string): ProcessInfo[] {
  const [processes, setProcesses] = useState<ProcessInfo[]>([]);
  useEffect(() => {
    if (ipc === undefined) return undefined;
    let live = true;
    let unlisten: () => void = () => undefined;
    void ipc
      .onEvent((event) => {
        if (event.workspace_id !== workspaceId || event.kind === 'process_output') return;
        setProcesses((current) => upsert(current, event.payload));
      })
      .then((off) => {
        if (!live) off();
        else unlisten = off;
        return ipc.list(workspaceId);
      })
      .then(
        (listed) => {
          // Events heard while the list was in flight are newer than it.
          if (live) setProcesses((current) => current.reduce(upsert, listed));
        },
        () => undefined,
      );
    return () => {
      live = false;
      unlisten();
    };
  }, [ipc, workspaceId]);
  return processes;
}

function ProcessRow({ process, chosen, onChoose, ipc, workspaceId }: { process: ProcessInfo; chosen: boolean; onChoose: () => void; ipc: ProcessIpc; workspaceId: string }): React.JSX.Element {
  const running = process.status === 'running';
  const id = process.process_id;
  return (
    <Box component="li" data-testid="process-row" sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
      <ButtonBase
        aria-pressed={chosen}
        onClick={onChoose}
        sx={(theme: Theme) => ({
          flex: 1,
          minWidth: 0,
          justifyContent: 'flex-start',
          gap: 1,
          paddingX: 0.5,
          borderRadius: theme.vars.shape.radiusSm,
          background: chosen ? theme.vars.palette.background.button.drawerMenu.hover : 'transparent',
        })}
      >
        <Typography
          variant="bodySmall"
          component="span"
          title={process.command}
          sx={(theme: Theme) => ({ fontFamily: theme.typography.fontFamilyMono, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' })}
        >
          {process.command}
        </Typography>
        <Typography variant="bodySmall" component="span" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics, flexShrink: 0 })}>
          {statusLabel(process)}
        </Typography>
      </ButtonBase>
      {running && (
        <>
          <Button size="small" onClick={() => void ipc.pin(workspaceId, id, !process.pinned).catch(() => undefined)}>
            {process.pinned ? t('workspace.processes.unpin', 'Unpin') : t('workspace.processes.pin', 'Pin')}
          </Button>
          <Button size="small" color="error" onClick={() => void ipc.kill(workspaceId, id).catch(() => undefined)}>
            {t('workspace.processes.stop', 'Stop')}
          </Button>
        </>
      )}
    </Box>
  );
}

export function TerminalPane({ workspaceId, ipc: given }: TerminalPaneProps): React.JSX.Element {
  const ipc = useMemo(() => given ?? tauriProcessIpc(), [given]);
  const processes = useProcesses(ipc, workspaceId);
  const [chosenId, setChosenId] = useState<string | null>(null);
  const newestFirst = [...processes].reverse();
  const chosen = newestFirst.find((process) => process.process_id === chosenId) ?? newestFirst.find((process) => process.status === 'running') ?? newestFirst[0];
  if (ipc === undefined || chosen === undefined) {
    return (
      <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
        {t('workspace.processes.none', 'No background processes in this folder yet.')}
      </Typography>
    );
  }
  return (
    <Box data-testid="terminal-pane" sx={{ display: 'flex', flexDirection: 'column', gap: 1, flex: 1, minHeight: 0 }}>
      <Box component="ul" aria-label={t('workspace.processes.list', 'Background processes')} sx={{ margin: 0, padding: 0, listStyle: 'none', display: 'flex', flexDirection: 'column', gap: 0.25 }}>
        {newestFirst.map((process) => (
          <ProcessRow
            key={process.process_id}
            process={process}
            chosen={process.process_id === chosen.process_id}
            onChoose={() => setChosenId(process.process_id)}
            ipc={ipc}
            workspaceId={workspaceId}
          />
        ))}
      </Box>
      <TerminalView key={chosen.process_id} ipc={ipc} workspaceId={workspaceId} process={chosen} />
    </Box>
  );
}
