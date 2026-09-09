import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useState } from 'react';

import CloseIcon from '@mui/icons-material/Close';
import CloudDownloadOutlinedIcon from '@mui/icons-material/CloudDownloadOutlined';
import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import OpenInNewOutlinedIcon from '@mui/icons-material/OpenInNewOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import IconButton from '@mui/material/IconButton';
import ToggleButton from '@mui/material/ToggleButton';
import ToggleButtonGroup from '@mui/material/ToggleButtonGroup';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { fetchArtifactBlob, uploadArtifactObject } from '@/shared/api/artifacts';
import { getConfig } from '@/shared/config';
import { t } from '@/shared/i18n';
import { triggerBlobDownload } from '@/shared/lib/download';

import { artifactPreviewKind, isArtifactPreviewableSize } from '../lib/artifactParsers';
import type { ArtifactListItem } from '../model/types';
import { ArtifactPreviewContent } from './ArtifactPreviewContent';

interface FilePreviewCanvasProps {
  readonly file: ArtifactListItem;
  readonly projectId: string;
  readonly bucket: string;
  readonly onClose: () => void;
  readonly onDelete: (key: string) => Promise<unknown>;
  readonly onSaved: () => unknown;
  readonly onUnsavedChangesUpdate?: (hasChanges: boolean) => void;
  /**
   * Opens this file in the richer canvas editor (issue #878) — syntax
   * highlighting, the mermaid split-view, the table grid — instead of the
   * plain text area below. Omitted, no such control renders (the CALLER
   * decides eligibility by filename, since this component's own `kind`
   * mapping is narrower than canvas's and the two must not silently drift).
   */
  readonly onOpenInCanvas?: () => void;
}

// oxlint-disable-next-line complexity -- this is the preview state machine across text, image, unavailable, save, and delete modes.
export function FilePreviewCanvas(props: FilePreviewCanvasProps): ReactNode {
  const kind = useMemo(() => artifactPreviewKind(props.file.name), [props.file.name]);
  const isOversized = useMemo(() => !isArtifactPreviewableSize(props.file.size, kind), [kind, props.file.size]);
  const supportsRendered = kind === 'markdown' || kind === 'csv' || kind === 'tsv' || kind === 'mermaid';
  const needsContent = kind !== 'docx' && kind !== 'unsupported' && !isOversized;
  const [mode, setMode] = useState<'code' | 'rendered'>(supportsRendered || kind === 'image' ? 'rendered' : 'code');
  const [originalContent, setOriginalContent] = useState('');
  const [editedContent, setEditedContent] = useState('');
  const [imageUrl, setImageUrl] = useState<string>();
  const [loading, setLoading] = useState(needsContent);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string>();
  const [closeConfirmation, setCloseConfirmation] = useState(false);
  const [deleteConfirmation, setDeleteConfirmation] = useState(false);
  const hasChanges = editedContent !== originalContent;

  const onUnsavedChangesUpdate = props.onUnsavedChangesUpdate;
  useEffect(() => onUnsavedChangesUpdate?.(hasChanges), [hasChanges, onUnsavedChangesUpdate]);

  useEffect(() => {
    setMode(supportsRendered || kind === 'image' ? 'rendered' : 'code');
    setOriginalContent('');
    setEditedContent('');
    setError(undefined);
    setLoading(needsContent);
    if (!needsContent) return;
    const controller = new AbortController();
    const config = getConfig();
    if (config.status !== 'ok') {
      setError('Runtime configuration is unavailable.');
      setLoading(false);
      return;
    }
    let active = true;
    void fetchArtifactBlob({
      baseUrl: config.config.vite_server_url,
      projectId: props.projectId,
      bucket: props.bucket,
      filePath: props.file.key,
      signal: controller.signal,
    }).then(async (result) => {
      if (!active) return;
      if (!result.ok) {
        if (result.error.kind !== 'aborted') setError('Failed to load file content.');
        return;
      }
      if (kind === 'image') {
        const url = URL.createObjectURL(result.data);
        if (!active) {
          URL.revokeObjectURL(url);
          return;
        }
        setImageUrl((previous) => {
          if (previous !== undefined) URL.revokeObjectURL(previous);
          return url;
        });
      } else {
        const content = await result.data.text();
        if (!active) return;
        setOriginalContent(content);
        setEditedContent(content);
      }
    }).catch(() => {
      if (active) setError('Failed to load file content.');
    }).finally(() => {
      if (active) setLoading(false);
    });
    return () => {
      active = false;
      controller.abort();
    };
    // `props.file.size`/`.lastModified`, not just `.key`/`.bucket`: a save
    // that overwrites this SAME object — the canvas editor's own "Save to
    // artifacts" (issue #878) route, or this component's own inline Save —
    // changes neither the key nor the bucket, so without one of these the
    // effect never re-ran and the preview (and a canvas reopened from it)
    // kept showing the bytes fetched when the file was first opened, not
    // what was just saved (#882 CI).
  }, [
    kind,
    needsContent,
    props.bucket,
    props.file.key,
    props.file.size,
    props.file.lastModified,
    props.projectId,
    supportsRendered,
  ]);

  useEffect(() => () => {
    if (imageUrl !== undefined) URL.revokeObjectURL(imageUrl);
  }, [imageUrl]);

  const download = useCallback(async () => {
    const config = getConfig();
    if (config.status !== 'ok') {
      setError('Runtime configuration is unavailable.');
      return;
    }
    const result = await fetchArtifactBlob({
      baseUrl: config.config.vite_server_url,
      projectId: props.projectId,
      bucket: props.bucket,
      filePath: props.file.key,
    });
    if (result.ok) triggerBlobDownload(result.data, props.file.name);
    else setError('Failed to download file.');
  }, [props.bucket, props.file.key, props.file.name, props.projectId]);

  const save = useCallback(async () => {
    if (!hasChanges || kind === 'image' || kind === 'docx' || kind === 'unsupported' || isOversized) return;
    const config = getConfig();
    if (config.status !== 'ok') {
      setError('Runtime configuration is unavailable.');
      return;
    }
    setSaving(true);
    setError(undefined);
    const blob = new Blob([editedContent], { type: 'text/plain' });
    const result = await uploadArtifactObject({
      baseUrl: config.config.vite_server_url,
      projectId: props.projectId,
      bucket: props.bucket,
      fileKey: props.file.key,
      file: blob,
    });
    setSaving(false);
    if (!result.ok) {
      setError('Failed to save file.');
      return;
    }
    setOriginalContent(editedContent);
    await props.onSaved();
  }, [editedContent, hasChanges, isOversized, kind, props]);

  const requestClose = (): void => {
    if (hasChanges) setCloseConfirmation(true);
    else props.onClose();
  };

  return (
    <Box sx={rootSx}>
      <Box sx={headerSx}>
        <Box>
          <Typography variant="headingSmall">{props.file.name}</Typography>
          <Typography variant="bodySmall">{props.bucket} / {props.file.key}</Typography>
        </Box>
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
          {supportsRendered && (
            <ToggleButtonGroup
              exclusive
              size="small"
              value={mode}
              onChange={(_event, value: 'code' | 'rendered' | null) => {
                if (value !== null) setMode(value);
              }}
            >
              <ToggleButton value="code">{t('artifacts.preview.code', 'Code')}</ToggleButton>
              <ToggleButton value="rendered">{t('artifacts.preview.rendered', 'Rendered')}</ToggleButton>
            </ToggleButtonGroup>
          )}
          {kind !== 'image' && kind !== 'docx' && kind !== 'unsupported' && !isOversized && (
            <>
              <Button
                disabled={!hasChanges || saving}
                onClick={() => setEditedContent(originalContent)}
              >
                {t('common.discard', 'Discard')}
              </Button>
              <Button
                variant="contained"
                disabled={!hasChanges || saving}
                onClick={() => void save()}
              >
                {saving ? t('common.saving', 'Saving…') : t('common.save', 'Save')}
              </Button>
            </>
          )}
          {props.onOpenInCanvas && (
            <Tooltip title={t('artifacts.preview.openInCanvas', 'Open in canvas')}>
              <IconButton
                aria-label={t('artifacts.preview.openInCanvasAria', 'Open in canvas')}
                onClick={props.onOpenInCanvas}
                data-testid="artifacts-open-in-canvas"
              >
                <OpenInNewOutlinedIcon />
              </IconButton>
            </Tooltip>
          )}
          <Tooltip title={t('common.download', 'Download')}>
            <IconButton
              aria-label={t('artifacts.preview.downloadAria', 'Download file')}
              onClick={() => void download()}
            >
              <CloudDownloadOutlinedIcon />
            </IconButton>
          </Tooltip>
          <Tooltip title={t('common.delete', 'Delete')}>
            <IconButton
              aria-label={t('artifacts.preview.deleteAria', 'Delete file')}
              onClick={() => setDeleteConfirmation(true)}
            >
              <DeleteOutlinedIcon />
            </IconButton>
          </Tooltip>
          <IconButton
            aria-label={t('artifacts.preview.closeAria', 'Close preview')}
            onClick={requestClose}
          >
            <CloseIcon />
          </IconButton>
        </Box>
      </Box>
      {error !== undefined && <Typography role="alert" sx={{ p: 2 }}>{error}</Typography>}
      <Box sx={contentSx}>
        {loading ? (
          <CircularProgress aria-label={t('artifacts.preview.loadingAria', 'Loading file content')} />
        ) : (
          <ArtifactPreviewContent
            kind={kind}
            filename={props.file.name}
            content={editedContent}
            {...(imageUrl === undefined ? {} : { imageUrl })}
            mode={mode}
            isOversized={isOversized}
            onChange={setEditedContent}
            onDownload={() => void download()}
          />
        )}
      </Box>
      <Dialog
        open={closeConfirmation}
        onClose={() => setCloseConfirmation(false)}
      >
        <DialogTitle>{t('artifacts.preview.discardTitle', 'Discard unsaved changes?')}</DialogTitle>
        <DialogContent>
          {t('artifacts.preview.discardDescription', 'Your edits to {{name}} will be lost.', {
            name: props.file.name,
          })}
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setCloseConfirmation(false)}>
            {t('artifacts.preview.keepEditing', 'Keep editing')}
          </Button>
          <Button
            color="warning"
            onClick={props.onClose}
          >
            {t('common.discard', 'Discard')}
          </Button>
        </DialogActions>
      </Dialog>
      <Dialog
        open={deleteConfirmation}
        onClose={() => setDeleteConfirmation(false)}
      >
        <DialogTitle>{t('artifacts.preview.deleteTitle', 'Delete file?')}</DialogTitle>
        <DialogContent>
          {t('artifacts.preview.deleteDescription', 'This will permanently remove {{name}}.', {
            name: props.file.name,
          })}
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setDeleteConfirmation(false)}>{t('common.cancel', 'Cancel')}</Button>
          <Button
            color="error"
            variant="contained"
            onClick={() => {
              void props.onDelete(props.file.key)
                .then(props.onClose)
                .catch(() => setError('Failed to delete file.'));
            }}
          >
            {t('common.delete', 'Delete')}
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}

const rootSx: SxProps<Theme> = { height: '100%', display: 'flex', flexDirection: 'column', overflow: 'hidden' };
const headerSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  gap: theme.spacing(2),
  padding: theme.spacing(2),
  borderBottom: `0.0625rem solid ${theme.vars.palette.border.lines}`,
});
const contentSx: SxProps<Theme> = (theme) => ({
  flex: 1,
  overflow: 'auto',
  display: 'grid',
  placeItems: 'stretch',
  padding: theme.spacing(2),
  backgroundColor: theme.vars.palette.background.secondary,
});
