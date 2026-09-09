/**
 * ui/canvas/SaveToArtifactsDialog.tsx — "Save to artifacts" from the canvas
 * header (issue #878): choose a bucket, confirm/rename the filename, and
 * write the document into the artifact store — the same route the artifacts
 * browser's own upload uses.
 *
 * Two save shapes, one dialog:
 *  - a canvas with no `source` yet (made from a chat turn, never saved
 *    before) picks a bucket and a filename from scratch;
 *  - a canvas OPENED from a stored file has a `source` already, and the
 *    dialog opens pre-filled with it — the ordinary "just Save" case still
 *    goes through this same confirm-and-write path, it only skips typing.
 *
 * The overwrite check is a plain existence read (`artifactObjectExists`), not
 * an ETag precondition: the artifact upload route takes no `If-Match` today,
 * so a real conflict is still last-write-wins underneath this — the dialog's
 * job is to stop an ACCIDENTAL overwrite of a same-named file the user did
 * not mean to replace, not to arbitrate a race with another editor.
 */
import { useCallback, useEffect, useState } from 'react';

import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { artifactObjectExists, listArtifactBucketNames, saveCanvasToArtifact } from '../../model/canvasFileTransfer';
import type { CanvasFileSource } from '../../lib/canvasFileSource';

export interface SaveToArtifactsDialogProps {
  readonly open: boolean;
  readonly projectId: string;
  /** The document AS IT STANDS RIGHT NOW — read live by the caller, the same rule `CanvasEditor`'s own Save/Copy follow. */
  readonly content: string;
  /** Where this canvas was opened from, or last saved to, if either has happened. Pre-fills the picker; omitted, the dialog opens with no bucket chosen and an empty filename. */
  readonly source?: CanvasFileSource | undefined;
  /** A filename to suggest when there is no `source` to prefill from (derived from the canvas's own title/language). */
  readonly suggestedName?: string | undefined;
  readonly onClose: () => void;
  /** Fires once the write has actually landed — the caller updates the canvas's own `source` from this. */
  readonly onSaved: (source: CanvasFileSource) => void;
}

type Phase = 'idle' | 'checking' | 'confirm-overwrite' | 'saving' | 'error';

export function SaveToArtifactsDialog({
  open,
  projectId,
  content,
  source,
  suggestedName,
  onClose,
  onSaved,
}: SaveToArtifactsDialogProps): React.ReactElement {
  const [buckets, setBuckets] = useState<readonly string[]>([]);
  const [bucket, setBucket] = useState(source?.bucket ?? '');
  const [name, setName] = useState(source?.name ?? suggestedName ?? '');
  const [phase, setPhase] = useState<Phase>('idle');
  const [error, setError] = useState<string | undefined>(undefined);

  useEffect(() => {
    if (!open) return;
    setBucket(source?.bucket ?? '');
    setName(source?.name ?? suggestedName ?? '');
    setPhase('idle');
    setError(undefined);
    let active = true;
    void listArtifactBucketNames(projectId).then((names) => {
      if (!active) return;
      setBuckets(names);
      setBucket((current) => (current !== '' ? current : (names[0] ?? '')));
    });
    return () => {
      active = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- re-seeds from `source`/`suggestedName` only when the dialog transitions to OPEN, matching `FilePreviewCanvas`'s own re-seed-on-identity-change convention.
  }, [open, projectId]);

  const canSubmit = bucket !== '' && name.trim() !== '' && (phase === 'idle' || phase === 'error');

  const write = useCallback(async () => {
    setPhase('saving');
    setError(undefined);
    const result = await saveCanvasToArtifact({ projectId, bucket, name: name.trim(), content });
    if (!result.ok) {
      setPhase('error');
      setError(t('features.chatMessages.canvas.saveToArtifacts.uploadFailed', 'Failed to save the file.'));
      return;
    }
    onSaved({ bucket, name: name.trim() });
  }, [projectId, bucket, name, content, onSaved]);

  const onSubmit = useCallback(async () => {
    if (!canSubmit) return;
    setPhase('checking');
    setError(undefined);
    // Overwriting the SAME object a file-backed canvas was opened from is the
    // ordinary "Save" case, not a surprise — only a NEW target name/bucket
    // collision needs the confirm.
    const isSameAsSource = source?.bucket === bucket && source.name === name.trim();
    if (!isSameAsSource && (await artifactObjectExists(projectId, bucket, name.trim()))) {
      setPhase('confirm-overwrite');
      return;
    }
    await write();
  }, [canSubmit, projectId, bucket, name, source, write]);

  const busy = phase === 'checking' || phase === 'saving';

  return (
    <Dialog
      open={open}
      onClose={busy ? undefined : onClose}
      data-testid="canvas-save-to-artifacts-dialog"
    >
      <DialogTitle>{t('features.chatMessages.canvas.saveToArtifacts.title', 'Save to artifacts')}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: 2, minWidth: '22rem', pt: 1 }}>
        <TextField
          select
          label={t('features.chatMessages.canvas.saveToArtifacts.bucket', 'Bucket')}
          value={bucket}
          onChange={(event) => setBucket(event.target.value)}
          disabled={busy || buckets.length === 0}
          slotProps={{ htmlInput: { 'data-testid': 'canvas-save-bucket-select' } }}
          fullWidth
        >
          {buckets.map((option) => (
            <MenuItem key={option} value={option}>
              {option}
            </MenuItem>
          ))}
        </TextField>
        <TextField
          label={t('features.chatMessages.canvas.saveToArtifacts.filename', 'File name')}
          value={name}
          onChange={(event) => setName(event.target.value)}
          disabled={busy}
          slotProps={{ htmlInput: { 'data-testid': 'canvas-save-filename-input' } }}
          fullWidth
        />
        {phase === 'confirm-overwrite' && (
          <Typography
            role="alert"
            color="warning.main"
            variant="bodySmall"
            data-testid="canvas-save-overwrite-warning"
          >
            {t(
              'features.chatMessages.canvas.saveToArtifacts.overwriteWarning',
              '{{name}} already exists in {{bucket}}. Saving will replace it.',
              { name: name.trim(), bucket },
            )}
          </Typography>
        )}
        {error !== undefined && (
          <Typography role="alert" color="error">
            {error}
          </Typography>
        )}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose} disabled={busy}>
          {t('common.cancel', 'Cancel')}
        </Button>
        {phase === 'confirm-overwrite' ? (
          <Button
            variant="contained"
            color="warning"
            onClick={() => void write()}
            data-testid="canvas-save-confirm-overwrite"
          >
            {t('features.chatMessages.canvas.saveToArtifacts.overwrite', 'Overwrite')}
          </Button>
        ) : (
          <Button
            variant="contained"
            onClick={() => void onSubmit()}
            disabled={!canSubmit}
            data-testid="canvas-save-submit"
          >
            {busy ? <CircularProgress size={16} /> : t('common.save', 'Save')}
          </Button>
        )}
      </DialogActions>
    </Dialog>
  );
}
