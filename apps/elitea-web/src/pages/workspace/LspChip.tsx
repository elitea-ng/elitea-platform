/**
 * The composer footer's language-server icon (#1236), a 16px mark beside the
 * context ring whose tooltip and accessible name say which languages' servers
 * give this folder's edits their diagnostics, seeded from `lsp_status` when it
 * mounts and kept current by `lsp://event` (IPC.md "Language servers"); an
 * event older than what a language shows (a smaller `seq`) is dropped. Nothing
 * shows until a server starts or is found missing; a stopped one goes. The
 * servers and their sandbox are the host's: this only shows their state.
 * Loaded lazily by `ThreadComposer`.
 */
import { useEffect, useState } from 'react';

import DataObjectOutlinedIcon from '@mui/icons-material/DataObjectOutlined';
import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';

import { lspStatus, onLspEvent, tauriLspInvoke, tauriLspListen, type LspEvent, type LspLanguage, type LspSnapshot, type LspState } from '@/shared/desktop/lspIpc';
import { t } from '@/shared/i18n';

/** Each language's latest state and the `seq` it is as of (a stopped one is kept, unshown, for its `seq`). */
type Shown = Partial<Record<LspLanguage, { state: LspState; message?: string | undefined; seq: number }>>;

function languageLabel(language: LspLanguage): string {
  switch (language) {
    case 'rust':
      return t('workspace.lsp.language.rust', 'Rust');
    case 'typescript':
      return t('workspace.lsp.language.typescript', 'TypeScript');
    case 'python':
      return t('workspace.lsp.language.python', 'Python');
    case 'go':
      return t('workspace.lsp.language.go', 'Go');
  }
}

function stateLabel(state: LspState): string {
  switch (state) {
    case 'starting':
      return t('workspace.lsp.state.starting', 'starting');
    case 'ready':
      return t('workspace.lsp.state.ready', 'ready');
    case 'unavailable':
      return t('workspace.lsp.state.unavailable', 'off');
    case 'crashed':
      return t('workspace.lsp.state.crashed', 'crashed');
    case 'stopped':
      return t('workspace.lsp.state.stopped', 'stopped');
  }
}

/** The statuses after `event`, for `workspaceId`'s chip; an event older than the language's state changes nothing. */
export function applyLspEvent(shown: Shown, workspaceId: string, event: LspEvent): Shown {
  if (event.workspace_id !== workspaceId || event.kind !== 'lsp_status') return shown;
  const { language, state, message } = event.payload;
  const current = shown[language];
  if (current !== undefined && event.seq < current.seq) return shown;
  return { ...shown, [language]: { state, message, seq: event.seq } };
}

/** The statuses after the host's `lsp_status` answer: it is the truth as of its `seq`, unless an event since says otherwise. */
export function applyLspSnapshot(shown: Shown, snapshot: LspSnapshot): Shown {
  const next: Shown = {};
  for (const [language, entry] of Object.entries(shown) as [LspLanguage, NonNullable<Shown[LspLanguage]>][]) {
    if (entry.seq >= snapshot.seq) next[language] = entry;
  }
  for (const { language, state, message } of snapshot.statuses) {
    if (next[language] === undefined) next[language] = { state, message, seq: snapshot.seq - 1 };
  }
  return next;
}

export default function LspChip({ workspaceId }: { workspaceId: string }): React.JSX.Element | null {
  const [shown, setShown] = useState<Shown>({});
  useEffect(() => {
    const listen = tauriLspListen();
    if (listen === undefined) return undefined;
    let unsubscribe: (() => void) | undefined;
    let live = true;
    void onLspEvent(listen, (event) => setShown((current) => applyLspEvent(current, workspaceId, event))).then((stop) => {
      if (live) unsubscribe = stop;
      else stop();
      // Asked once the stream is live, so no change falls between the two.
      const invoke = tauriLspInvoke();
      if (!live || invoke === undefined) return;
      lspStatus(invoke, workspaceId).then(
        (snapshot) => {
          if (live) setShown((current) => applyLspSnapshot(current, snapshot));
        },
        () => undefined,
      );
    });
    return () => {
      live = false;
      unsubscribe?.();
    };
  }, [workspaceId]);
  const entries = (Object.entries(shown) as [LspLanguage, NonNullable<Shown[LspLanguage]>][]).filter(([, status]) => status.state !== 'stopped');
  if (entries.length === 0) return null;
  const label = entries.map(([language, status]) => `${languageLabel(language)} ${stateLabel(status.state)}`).join(' · ');
  const details = entries.map(([language, status]) => status.message ?? `${languageLabel(language)}: ${stateLabel(status.state)}`).join('\n');
  const troubled = entries.some(([, status]) => status.state === 'crashed');
  return (
    <Tooltip title={<Box sx={{ whiteSpace: 'pre-line' }}>{`${label}\n`}{t('workspace.lsp.tooltip', 'Diagnostics after edits, from language servers on this computer:')}{`\n${details}`}</Box>}>
      <Box
        component="span"
        data-testid="lsp-status"
        sx={(theme: Theme) => ({
          display: 'inline-flex',
          flexShrink: 0,
          color: troubled ? theme.vars.palette.error.main : theme.vars.palette.text.metrics,
          '& svg': { width: '1rem', height: '1rem' },
        })}
      >
        <DataObjectOutlinedIcon titleAccess={t('workspace.lsp.aria', 'Language servers: {{label}}', { label })} />
      </Box>
    </Tooltip>
  );
}
