/**
 * The folder's index as a 16px mark in the composer footer, beside the
 * language-server icon: its colour says the state (quiet when ready or off, the
 * accent while building, a warning when stale, an error colour on failure),
 * its tooltip says it in words ("Index ready · 257.3K entities"), and a click
 * opens the inspector's Index tab, where the settings and progress are. Reads
 * the same query the Index tab does, so it costs no extra request. Nothing
 * outside the desktop host.
 */
import AccountTreeOutlinedIcon from '@mui/icons-material/AccountTreeOutlined';
import ButtonBase from '@mui/material/ButtonBase';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';

import type { IndexIpc } from '@/shared/desktop/indexIpc';
import { t } from '@/shared/i18n';

import { indexLine } from '../model/indexLine';
import { useIndexIpc } from '../model/ipcContext';
import { useWorkspaceIndex, type IndexView } from '../model/useWorkspaceIndex';

type Tone = 'quiet' | 'busy' | 'warn' | 'error';

function describe(view: IndexView | undefined): { text: string; tone: Tone } {
  if (view === undefined) return { text: t('workspace.indexIcon.reading', 'Index: reading…'), tone: 'quiet' };
  if (view.kind === 'disabled') return { text: t('workspace.indexIcon.off', 'Index off: the agent searches files without it'), tone: 'quiet' };
  switch (view.status.state) {
    case 'building':
      return { text: t('workspace.indexIcon.building', 'Index building…'), tone: 'busy' };
    case 'off':
      return { text: t('workspace.indexIcon.off', 'Index off: the agent searches files without it'), tone: 'quiet' };
    case 'stale':
    case 'stale_policy':
      return { text: indexLine(view) ?? '', tone: 'warn' };
    case 'error':
      return { text: indexLine(view) ?? '', tone: 'error' };
    case 'ready':
      return { text: indexLine(view) ?? '', tone: 'quiet' };
  }
}

function toneColor(theme: Theme, tone: Tone): string {
  switch (tone) {
    case 'busy':
      return theme.vars.palette.primary.main;
    case 'warn':
      return theme.vars.palette.warning.main;
    case 'error':
      return theme.vars.palette.error.main;
    case 'quiet':
      return theme.vars.palette.text.metrics;
  }
}

function Mark({ ipc, workspaceId, onOpen }: IndexStatusIconProps & { ipc: IndexIpc }): React.JSX.Element {
  const index = useWorkspaceIndex(ipc, workspaceId, { open: true });
  const { text, tone } = describe(index.view);
  const hint = t('workspace.indexIcon.open', 'Click to open the Index tab');
  return (
    <Tooltip title={`${text}. ${hint}`}>
      <ButtonBase
        data-testid="index-status"
        data-tone={tone}
        aria-label={text}
        onClick={onOpen}
        sx={(theme: Theme) => ({
          display: 'inline-flex',
          flexShrink: 0,
          borderRadius: theme.vars.shape.radiusSm,
          color: toneColor(theme, tone),
          '& svg': { width: '1rem', height: '1rem' },
          '&:hover': { color: theme.vars.palette.text.secondary },
        })}
      >
        <AccountTreeOutlinedIcon />
      </ButtonBase>
    </Tooltip>
  );
}

export interface IndexStatusIconProps {
  workspaceId: string;
  /** Open the inspector's Index tab. */
  onOpen: () => void;
}

export function IndexStatusIcon(props: IndexStatusIconProps): React.JSX.Element | null {
  const ipc = useIndexIpc();
  if (ipc === undefined) return null;
  return <Mark ipc={ipc} {...props} />;
}
