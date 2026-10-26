/**
 * Files changed (ADR-0034 Phase 2): what a cloud thread's turns changed,
 * since the handoff or turn by turn, with each file's status and +/- lines,
 * and the diff the worker kept for it. A binary or too-large file is listed
 * without one. Loaded only when the dialog opens.
 */
import { useId, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Collapse from '@mui/material/Collapse';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import MenuItem from '@mui/material/MenuItem';
import type { Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { WorkspaceChangedFile, WorkspaceChangeSet, WorkspaceHandoffChanges } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { parseUnifiedDiff } from '@/shared/lib/unifiedDiff';
import { DiffView } from '@/shared/ui/DiffView';

import { scopeView, turnNumber, useCloudChanges, useCloudDiff, type CloudScope, type DiffTarget, type ScopeView } from './cloudChanges';

export interface CloudFilesChangedProps {
  readonly projectId: string;
  readonly conversationUuid: string;
  readonly open: boolean;
  readonly onClose: () => void;
}

const mutedSx = (theme: Theme) => ({ color: theme.vars.palette.text.secondary });

function statusLabel(status: string): string {
  switch (status) {
    case 'added':
      return t('pages.chat.cloud.files.added', 'Added');
    case 'deleted':
      return t('pages.chat.cloud.files.deleted', 'Deleted');
    case 'renamed':
      return t('pages.chat.cloud.files.renamed', 'Renamed');
    default:
      return t('pages.chat.cloud.files.modified', 'Modified');
  }
}

function pathLabel(file: WorkspaceChangedFile): string {
  if (file.status === 'renamed' && typeof file.from_path === 'string') return `${file.from_path} → ${file.path}`;
  return file.path;
}

function DiffPanel({ projectId, conversationUuid, target }: { projectId: string; conversationUuid: string; target: DiffTarget }): React.JSX.Element {
  const diff = useCloudDiff(projectId, conversationUuid, target);
  if (diff.isPending) return <Typography variant="bodySmall">{t('pages.chat.cloud.files.diffLoading', 'Loading the diff…')}</Typography>;
  if (diff.isError) return <Alert severity="error">{t('pages.chat.cloud.files.diffFailed', 'The diff could not be read.')}</Alert>;
  return <DiffView data-testid="cloud-file-diff" parts={parseUnifiedDiff(diff.data)} />;
}

interface FileRowProps {
  readonly file: WorkspaceChangedFile;
  readonly projectId: string;
  readonly conversationUuid: string;
  readonly view: ScopeView;
}

function FileRow({ file, projectId, conversationUuid, view }: FileRowProps): React.JSX.Element {
  const [open, setOpen] = useState(false);
  const counted = typeof file.additions === 'number' && typeof file.deletions === 'number';
  const target = view.commit === null ? null : { commit: view.commit, scope: view.scope, path: file.path };
  // A path may hold any character; the element id must not.
  const diffId = useId();
  return (
    <Box component="li" data-testid="cloud-changed-file" sx={(theme: Theme) => ({ listStyle: 'none', paddingY: 0.5, borderBottom: `1px solid ${theme.vars.palette.divider}` })}>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexWrap: 'wrap' }}>
        <Chip size="small" variant="outlined" label={statusLabel(file.status)} />
        <Typography variant="labelMedium" sx={{ wordBreak: 'break-all', minWidth: 0 }}>
          {pathLabel(file)}
        </Typography>
        {counted && <Typography variant="bodySmall" sx={mutedSx}>{`+${String(file.additions)} -${String(file.deletions)}`}</Typography>}
        {file.binary && <Chip size="small" label={t('pages.chat.cloud.files.binary', 'Binary')} />}
        {file.too_large && <Chip size="small" label={t('pages.chat.cloud.files.tooLarge', 'Too large to show')} />}
        {file.has_diff && target !== null && (
          <Button size="small" sx={{ marginLeft: 'auto' }} aria-expanded={open} aria-controls={diffId} onClick={() => setOpen((value) => !value)}>
            {open ? t('pages.chat.cloud.files.hideDiff', 'Hide diff') : t('pages.chat.cloud.files.showDiff', 'Show diff')}
          </Button>
        )}
      </Box>
      {target !== null && (
        <Collapse in={open} unmountOnExit>
          <Box id={diffId} sx={{ paddingTop: 1, overflowX: 'auto' }}>
            <DiffPanel projectId={projectId} conversationUuid={conversationUuid} target={target} />
          </Box>
        </Collapse>
      )}
    </Box>
  );
}

/** Why the list may not be all of it. */
function Notes({ changes, set }: { changes: WorkspaceHandoffChanges; set: WorkspaceChangeSet }): React.JSX.Element {
  return (
    <>
      {!changes.complete && (
        <Alert severity="info" data-testid="cloud-files-incomplete">
          {t('pages.chat.cloud.files.incomplete', 'The changes of some cloud turns were not recorded, so this list may not be complete.')}
        </Alert>
      )}
      {set.hidden_files > 0 && (
        <Alert severity="info" data-testid="cloud-files-hidden">
          {t('pages.chat.cloud.files.hidden', '{{n}} changed files are hidden by the exclusion rules.', { n: String(set.hidden_files) })}
        </Alert>
      )}
      {set.omitted_files > 0 && (
        <Alert severity="info" data-testid="cloud-files-omitted">
          {t('pages.chat.cloud.files.omitted', '{{n}} more changed files are not listed.', { n: String(set.omitted_files) })}
        </Alert>
      )}
    </>
  );
}

function ScopePicker({ changes, picked, onPick }: { changes: WorkspaceHandoffChanges; picked: CloudScope; onPick: (scope: CloudScope) => void }): React.JSX.Element {
  return (
    <TextField
      select
      size="small"
      label={t('pages.chat.cloud.files.scope', 'Show')}
      value={picked !== 'cumulative' && changes.turns.some((turn) => turn.commit === picked.commit) ? picked.commit : 'cumulative'}
      onChange={(event) => onPick(event.target.value === 'cumulative' ? 'cumulative' : { commit: event.target.value })}
      slotProps={{ htmlInput: { 'data-testid': 'cloud-files-scope' } }}
      sx={{ minWidth: '14rem', alignSelf: 'flex-start' }}
    >
      <MenuItem value="cumulative">{t('pages.chat.cloud.files.sinceHandoff', 'Since the handoff')}</MenuItem>
      {changes.turns.map((turn, index) => (
        <MenuItem key={turn.commit} value={turn.commit}>
          {t('pages.chat.cloud.files.turn', 'Turn {{n}}', { n: String(turnNumber(changes, index)) })}
        </MenuItem>
      ))}
    </TextField>
  );
}

function ChangeList({ projectId, conversationUuid, changes }: { projectId: string; conversationUuid: string; changes: WorkspaceHandoffChanges }): React.JSX.Element {
  const [picked, setPicked] = useState<CloudScope>('cumulative');
  const view = scopeView(changes, picked);
  const set = view.set;
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      {changes.turns.length > 0 && <ScopePicker changes={changes} picked={picked} onPick={setPicked} />}
      {set === null ? (
        <Typography variant="bodyMedium" data-testid="cloud-files-empty">
          {t('pages.chat.cloud.files.none', 'No changed files are recorded yet.')}
        </Typography>
      ) : (
        <>
          <Notes changes={changes} set={set} />
          <Typography variant="bodySmall" sx={mutedSx} data-testid="cloud-files-summary">
            {t('pages.chat.cloud.files.summary', '{{files}} files, +{{added}} -{{removed}}', { files: String(set.files.length), added: String(set.additions), removed: String(set.deletions) })}
          </Typography>
          {set.files.length === 0 ? (
            <Typography variant="bodyMedium" data-testid="cloud-files-empty">
              {t('pages.chat.cloud.files.noFiles', 'No files changed.')}
            </Typography>
          ) : (
            <Box component="ul" sx={{ margin: 0, padding: 0 }}>
              {set.files.map((file) => (
                <FileRow key={`${view.scope}-${view.commit ?? ''}-${file.path}`} file={file} projectId={projectId} conversationUuid={conversationUuid} view={view} />
              ))}
            </Box>
          )}
        </>
      )}
    </Box>
  );
}

export default function CloudFilesChanged({ projectId, conversationUuid, open, onClose }: CloudFilesChangedProps): React.JSX.Element {
  const changes = useCloudChanges(projectId, conversationUuid, open);
  return (
    <Dialog open={open} onClose={onClose} fullWidth maxWidth="md" aria-labelledby="cloud-files-title">
      <DialogTitle id="cloud-files-title">{t('pages.chat.cloud.files.title', 'Files changed in the cloud')}</DialogTitle>
      <DialogContent data-testid="cloud-files-changed">
        {changes.isPending && <Typography variant="bodyMedium">{t('pages.chat.cloud.files.loading', 'Loading the changes…')}</Typography>}
        {changes.isError && <Alert severity="error">{t('pages.chat.cloud.files.loadFailed', 'The changes could not be read.')}</Alert>}
        {changes.data !== undefined && <ChangeList projectId={projectId} conversationUuid={conversationUuid} changes={changes.data} />}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>{t('pages.chat.cloud.files.close', 'Close')}</Button>
      </DialogActions>
    </Dialog>
  );
}
