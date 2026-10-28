/**
 * The desktop inspector's Changes tab, light: one collapsible header per turn
 * (the prompt on one line, "3 files · +12 −12") and one dense row per file —
 * a glyph, the file name, its folder in the quiet colour, coloured counts and
 * a status letter (M/A/D/R), as VS Code's Source Control view, Zed's git panel
 * and GitHub Desktop list them. Actions (reveal, open, revert, undo the turn)
 * show on hover or keyboard focus only; clicking a row toggles its diff.
 */
import { useState, type MouseEvent, type ReactNode } from 'react';

import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import HistoryOutlinedIcon from '@mui/icons-material/HistoryOutlined';
import InsertDriveFileOutlinedIcon from '@mui/icons-material/InsertDriveFileOutlined';
import UndoOutlinedIcon from '@mui/icons-material/UndoOutlined';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Collapse from '@mui/material/Collapse';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import type { ChangedFile } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { parseUnifiedDiff } from '@/shared/lib/unifiedDiff';
import { DiffView } from '@/shared/ui/DiffView';

const ONE_LINE = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } as const;

/** `src/app/main.rs` → name `main.rs`, folder `src/app`. */
export function splitPath(path: string): { name: string; dir: string } {
  const at = path.lastIndexOf('/');
  return at < 0 ? { name: path, dir: '' } : { name: path.slice(at + 1), dir: path.slice(0, at) };
}

function statusLetter(status: ChangedFile['status']): { letter: string; label: string; color: (theme: Theme) => string } {
  switch (status) {
    case 'added':
      return { letter: 'A', label: t('workspace.changes.added', 'Added'), color: (theme) => theme.vars.palette.success.main };
    case 'modified':
      return { letter: 'M', label: t('workspace.changes.modified', 'Modified'), color: (theme) => theme.vars.palette.warning.main };
    case 'deleted':
      return { letter: 'D', label: t('workspace.changes.deleted', 'Deleted'), color: (theme) => theme.vars.palette.error.main };
    case 'renamed':
      return { letter: 'R', label: t('workspace.changes.renamed', 'Renamed'), color: (theme) => theme.vars.palette.info.main };
  }
}

/** `+12 −3`, green and red, small. */
function Counts({ added, removed }: { added: number; removed: number }): React.JSX.Element {
  return (
    <Typography variant="bodySmall" component="span" data-testid="change-counts" sx={{ flexShrink: 0, display: 'inline-flex', gap: 0.5, fontVariantNumeric: 'tabular-nums' }}>
      <Box component="span" sx={(theme: Theme) => ({ color: theme.vars.palette.success.main })}>{`+${String(added)}`}</Box>
      <Box component="span" sx={(theme: Theme) => ({ color: theme.vars.palette.error.main })}>{`−${String(removed)}`}</Box>
    </Typography>
  );
}

/**
 * A dense row: hover and focus tint it like the sidebar's rows; the actions
 * overlay the row's right edge (where the counts were) on hover or focus.
 * They stay in the tab order, so focusing one reveals it.
 */
function rowSx(theme: Theme, swaps: boolean) {
  return {
    position: 'relative',
    display: 'flex',
    alignItems: 'center',
    minHeight: '1.75rem',
    borderRadius: theme.vars.shape.radiusSm,
    '&:hover, &:focus-within': { background: theme.vars.palette.background.button.drawerMenu.hover },
    '& .changes-row-actions': { position: 'absolute', right: 0, top: 0, bottom: 0, display: 'flex', alignItems: 'center', gap: 0.25, paddingRight: 0.5, opacity: 0, pointerEvents: 'none' },
    '& .changes-row-actions button': { padding: 0.375, fontSize: '0.875rem' },
    '&:hover .changes-row-actions, &:focus-within .changes-row-actions': { opacity: 1, pointerEvents: 'auto' },
    ...(swaps ? { '&:hover .changes-row-meta, &:focus-within .changes-row-meta': { visibility: 'hidden' } } : {}),
  } as const;
}

const MAIN_SX = (theme: Theme) =>
  ({
    flex: 1,
    minWidth: 0,
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'flex-start',
    gap: 0.75,
    paddingY: 0.25,
    paddingX: 1,
    minHeight: '1.75rem',
    borderRadius: theme.vars.shape.radiusSm,
    textAlign: 'left',
  }) as const;

function Glyph({ children }: { children: ReactNode }): React.JSX.Element {
  return (
    <Box aria-hidden sx={(theme: Theme) => ({ display: 'flex', flexShrink: 0, color: theme.vars.palette.text.metrics, '& svg': { width: '0.875rem', height: '0.875rem' } })}>
      {children}
    </Box>
  );
}

export interface PanelFileRowProps {
  file: ChangedFile;
  busy: boolean;
  /** `undefined`: read-only (a recorded turn, or undone). */
  onRevert: (() => void) | undefined;
  fileActions: ((file: ChangedFile) => ReactNode) | undefined;
  onContextMenu: ((file: ChangedFile, event: MouseEvent) => void) | undefined;
}

/** One file: name, folder, counts, status letter; a click shows its diff below. */
export function PanelFileRow({ file, busy, onRevert, fileActions, onContextMenu }: PanelFileRowProps): React.JSX.Element {
  const [open, setOpen] = useState(false);
  const { name, dir } = splitPath(file.path);
  const status = statusLetter(file.status);
  const diffId = `diff-${file.path}`;
  const hasDiff = file.diff !== '';
  const content = (
    <>
      <Glyph>
        <InsertDriveFileOutlinedIcon />
      </Glyph>
      <Typography variant="labelSmall" component="span" sx={(theme: Theme) => ({ ...ONE_LINE, flexShrink: 0, maxWidth: '70%', color: theme.vars.palette.text.secondary })}>
        {name}
      </Typography>
      <Typography variant="bodySmall" component="span" sx={(theme: Theme) => ({ ...ONE_LINE, flex: 1, minWidth: 0, color: theme.vars.palette.text.metrics })}>
        {dir}
      </Typography>
      <Box component="span" className="changes-row-meta" sx={{ display: 'inline-flex', alignItems: 'center', gap: 1, flexShrink: 0 }}>
        <Counts added={file.added} removed={file.removed} />
        <Typography variant="labelSmall" component="span" aria-label={status.label} sx={(theme: Theme) => ({ width: '0.75rem', textAlign: 'center', fontWeight: 600, color: status.color(theme) })}>
          {status.letter}
        </Typography>
      </Box>
    </>
  );
  const actions = fileActions === undefined ? null : fileActions(file);
  const swaps = actions !== null || onRevert !== undefined;
  return (
    <Box component="li" data-testid="changed-file" sx={{ listStyle: 'none' }} onContextMenu={onContextMenu === undefined ? undefined : (event: MouseEvent) => onContextMenu(file, event)}>
      <Box sx={(theme: Theme) => rowSx(theme, swaps)}>
        {hasDiff ? (
          <ButtonBase
            data-testid="changed-file-toggle"
            title={file.path}
            aria-expanded={open}
            aria-controls={diffId}
            aria-label={t('workspace.changes.diffOf', 'Diff of {{path}}', { path: file.path })}
            onClick={() => setOpen((value) => !value)}
            sx={MAIN_SX}
          >
            {content}
          </ButtonBase>
        ) : (
          <Box title={file.path} sx={MAIN_SX}>
            {content}
          </Box>
        )}
        {swaps && (
          <Box className="changes-row-actions">
            {actions}
            {onRevert !== undefined && (
              <Tooltip title={t('workspace.changes.revert', 'Revert')}>
                <span>
                  <IconButton size="small" disabled={busy} onClick={onRevert} aria-label={t('workspace.changes.revertFile', 'Revert {{path}}', { path: file.path })}>
                    <UndoOutlinedIcon fontSize="inherit" />
                  </IconButton>
                </span>
              </Tooltip>
            )}
          </Box>
        )}
      </Box>
      {hasDiff && (
        <Collapse in={open} unmountOnExit>
          <Box id={diffId} sx={{ paddingX: 1, paddingY: 0.5, overflowX: 'auto' }}>
            <DiffView parts={parseUnifiedDiff(file.diff)} />
          </Box>
        </Collapse>
      )}
    </Box>
  );
}

export interface PanelTurnHeaderProps {
  /** The turn's prompt (empty or absent: "This turn"). */
  label: string | undefined;
  files: readonly ChangedFile[];
  expanded: boolean;
  onToggle: () => void;
  listId: string;
  /** `undefined`: read-only or undone, no undo. */
  onUndo: (() => void) | undefined;
  /** An older turn: "Restore folder to before this turn" instead of "Undo turn". */
  older: boolean;
  undoDisabled: boolean;
  undone: boolean;
}

/** The turn's one-line, collapsible header; undo shows on hover or focus. */
export function PanelTurnHeader({ label, files, expanded, onToggle, listId, onUndo, older, undoDisabled, undone }: PanelTurnHeaderProps): React.JSX.Element {
  const added = files.reduce((sum, file) => sum + file.added, 0);
  const removed = files.reduce((sum, file) => sum + file.removed, 0);
  const text = label === undefined || label === '' ? t('workspace.changes.thisTurn', 'This turn') : label;
  const undoLabel = older ? t('workspace.changes.restoreFolder', 'Restore folder to before this turn') : t('workspace.changes.undo', 'Undo turn');
  return (
    <Box component="h3" sx={(theme: Theme) => ({ ...rowSx(theme, onUndo !== undefined), margin: 0 })}>
      <ButtonBase data-testid="changes-turn-toggle" title={text} aria-expanded={expanded} aria-controls={listId} onClick={onToggle} sx={(theme: Theme) => ({ ...MAIN_SX(theme), paddingLeft: 0.5 })}>
        <Glyph>
          <ChevronRightIcon sx={{ transition: 'transform 120ms', transform: expanded ? 'rotate(90deg)' : 'none' }} />
        </Glyph>
        <Typography variant="labelSmall" component="span" sx={(theme: Theme) => ({ ...ONE_LINE, flex: 1, minWidth: 0, fontWeight: 600, color: theme.vars.palette.text.secondary })}>
          {text}
        </Typography>
        <Box component="span" className="changes-row-meta" sx={(theme: Theme) => ({ display: 'inline-flex', alignItems: 'center', gap: 0.5, flexShrink: 0, color: theme.vars.palette.text.metrics })}>
          <Typography variant="bodySmall" component="span">
            {undone
              ? t('workspace.changes.undone', 'Undone')
              : files.length === 1
                ? t('workspace.changes.fileCountOne', '1 file')
                : t('workspace.changes.fileCount', '{{n}} files', { n: files.length })}
          </Typography>
          {!undone && (
            <>
              <Typography variant="bodySmall" component="span" aria-hidden>
                ·
              </Typography>
              <Counts added={added} removed={removed} />
            </>
          )}
        </Box>
      </ButtonBase>
      {onUndo !== undefined && (
        <Box className="changes-row-actions">
          <Tooltip title={undoLabel}>
            <span>
              <IconButton size="small" disabled={undoDisabled} onClick={onUndo} aria-label={undoLabel}>
                {older ? <HistoryOutlinedIcon fontSize="inherit" /> : <UndoOutlinedIcon fontSize="inherit" />}
              </IconButton>
            </span>
          </Tooltip>
        </Box>
      )}
    </Box>
  );
}
