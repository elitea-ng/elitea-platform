import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';

import AddOutlinedIcon from '@mui/icons-material/AddOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useSearch } from '@tanstack/react-router';

import {
  ArtifactTable,
  buildArtifactTree,
  BucketSidebar,
  DuplicateResolutionDialog,
  expandFoldersToArtifactKeys,
  FilePreviewCanvas,
  getExpandedArtifactPaths,
  type ArtifactListItem,
  UploadPathDialog,
  useArtifactBuckets,
  useArtifactMutations,
  useArtifacts,
  useArtifactStorageConfigurations,
  useArtifactUpload,
  useZipDownload,
  ZipDownloadProgressDialog,
} from '@/features/artifacts';
import { formatArtifactSize } from '@/entities/artifact';
import { fetchArtifactBlob } from '@/shared/api/artifacts';
import { getConfig } from '@/shared/config';
import { t } from '@/shared/i18n';
import { triggerBlobDownload } from '@/shared/lib/download';

import { useSelectedProjectId } from './lib/useSelectedProjectId';

interface ArtifactsRouteSearch {
  readonly bucket?: string;
  readonly file?: string;
  readonly folder?: string;
  readonly shared_bucket?: string;
}

function trailingSlash(path: string): string {
  return path !== '' && !path.endsWith('/') ? `${path}/` : path;
}

function parentFolder(fileKey: string): string {
  const slash = fileKey.lastIndexOf('/');
  return slash < 0 ? '' : fileKey.slice(0, slash + 1);
}

// oxlint-disable-next-line complexity -- page orchestration covers URL restoration and mutually exclusive bucket/table/preview states.
export function Artifacts(): ReactNode {
  const navigate = useNavigate();
  const search = useSearch({ strict: false }) as ArtifactsRouteSearch;
  const projectId = useSelectedProjectId();
  const buckets = useArtifactBuckets(projectId);
  const storage = useArtifactStorageConfigurations(projectId);
  const selectedBucket = buckets.data?.find((bucket) => bucket.name === search.bucket);
  const files = useArtifacts(projectId, selectedBucket?.name);
  const mutations = useArtifactMutations(projectId);
  const zip = useZipDownload();
  const [selectedStorage, setSelectedStorage] = useState<string>();
  const [bucketsCollapsed, setBucketsCollapsed] = useState(false);
  const [pendingDelete, setPendingDelete] = useState<readonly ArtifactListItem[]>([]);
  const [hasUnsavedChanges, setHasUnsavedChanges] = useState(false);
  const [pendingNavigation, setPendingNavigation] = useState<(() => void)>();
  const [actionError, setActionError] = useState<string>();
  const currentPrefix = useMemo(
    () => trailingSlash(search.file ? parentFolder(search.file) : search.folder ?? ''),
    [search.file, search.folder],
  );
  const previewFile = useMemo<ArtifactListItem | undefined>(() => {
    if (!search.file) return undefined;
    const artifact = files.data?.find((candidate) => candidate.key === search.file);
    const name = search.file.split('/').filter(Boolean).pop() ?? search.file;
    return {
      id: search.file,
      key: search.file,
      name,
      kind: 'file',
      size: artifact?.size ?? 0,
      ...(artifact?.lastModified === undefined ? {} : { lastModified: artifact.lastModified }),
    };
  }, [files.data, search.file]);

  const bucketTree = useMemo(() => buildArtifactTree(files.data ?? []), [files.data]);
  const expandedPaths = useMemo(
    () => getExpandedArtifactPaths(search.file ?? currentPrefix),
    [currentPrefix, search.file],
  );
  const totalSize = useMemo(
    () => formatArtifactSize((buckets.data ?? []).reduce((sum, bucket) => sum + bucket.sizeBytes, 0)),
    [buckets.data],
  );

  const setSearch = useCallback((next: { bucket?: string; file?: string; folder?: string }) => {
    void navigate({
      to: '/artifacts',
      search: {
        bucket: next.bucket ?? '',
        file: next.file ?? '',
        folder: next.folder ?? '',
        shared_bucket: search.shared_bucket ?? '',
      },
    });
  }, [navigate, search.shared_bucket]);

  const requestNavigation = useCallback((action: () => void) => {
    if (hasUnsavedChanges) {
      setPendingNavigation(() => action);
      return;
    }
    action();
  }, [hasUnsavedChanges]);

  useEffect(() => {
    if (selectedStorage === undefined && storage.data?.[0] !== undefined) setSelectedStorage(storage.data[0].id);
  }, [selectedStorage, storage.data]);

  useEffect(() => {
    if (search.bucket || buckets.data?.[0] === undefined) return;
    setSearch({ bucket: buckets.data[0].name });
  }, [buckets.data, search.bucket, setSearch]);

  const upload = useArtifactUpload({
    ...(projectId === undefined ? {} : { projectId }),
    ...(selectedBucket === undefined ? {} : { bucket: selectedBucket.name }),
    contents: files.data ?? [],
    currentPrefix,
    // `refreshFiles`, not `files.refetch()`: the "Size:" footer sums the BUCKET list's `size_bytes`,
    // so refreshing the object list alone froze the total until the next page load (18 B after a
    // second, 35 B upload; 53 B after F5) — the delete mutations already invalidate both.
    onUploaded: () => mutations.refreshFiles(selectedBucket?.name ?? ''),
  });

  const downloadFile = useCallback(async (item: ArtifactListItem) => {
    if (projectId === undefined || selectedBucket === undefined) return;
    const config = getConfig();
    if (config.status !== 'ok') {
      setActionError('Runtime configuration is unavailable.');
      return;
    }
    const result = await fetchArtifactBlob({
      baseUrl: config.config.vite_server_url,
      projectId,
      bucket: selectedBucket.name,
      filePath: item.key,
    });
    if (result.ok) triggerBlobDownload(result.data, item.name);
    else setActionError(`Failed to download ${item.name}.`);
  }, [projectId, selectedBucket]);

  const downloadItems = useCallback((items: readonly ArtifactListItem[]) => {
    if (projectId === undefined || selectedBucket === undefined) return;
    const keys = expandFoldersToArtifactKeys(items, files.data ?? []);
    if (keys.length === 1 && items.length === 1 && items[0]?.kind === 'file') {
      void downloadFile(items[0]);
      return;
    }
    void zip.start({
      projectId,
      bucket: selectedBucket.name,
      filenames: keys,
      currentPrefix,
    });
  }, [currentPrefix, downloadFile, files.data, projectId, selectedBucket, zip]);

  const confirmDelete = (): void => {
    if (selectedBucket === undefined) return;
    const keys = expandFoldersToArtifactKeys(pendingDelete, files.data ?? []);
    if (keys.length === 0) {
      setPendingDelete([]);
      return;
    }
    void mutations.deleteMany
      .mutateAsync({ bucket: selectedBucket.name, keys })
      .then(() => setPendingDelete([]))
      .catch(() => setActionError('Failed to delete the selected artifacts.'));
  };

  const missingBucket = search.bucket !== undefined && search.bucket !== '' && !buckets.isFetching && selectedBucket === undefined;
  const queryError = buckets.isError ? 'Failed to load buckets.'
    : files.isError ? 'Failed to load artifacts.' : undefined;

  return (
    <Box sx={rootSx}>
      <Box sx={sidebarSx(bucketsCollapsed)}>
      <BucketSidebar
        projectId={projectId}
        buckets={buckets.data ?? []}
        {...(selectedBucket === undefined ? {} : { selectedBucket: selectedBucket.name })}
        storageConfigurations={storage.data ?? []}
        {...(selectedStorage === undefined ? {} : { selectedStorage })}
        loading={buckets.isFetching}
        collapsed={bucketsCollapsed}
        tree={selectedBucket === undefined ? [] : bucketTree}
        expandedPaths={expandedPaths}
        {...(search.file ? { selectedKey: search.file } : currentPrefix === '' ? {} : { selectedKey: currentPrefix })}
        totalSize={totalSize}
        onToggleCollapsed={() => setBucketsCollapsed((current) => !current)}
        onSelectFile={(item) => requestNavigation(() => setSearch({ bucket: selectedBucket?.name ?? '', file: item.key }))}
        onSelectFolder={(key) => requestNavigation(() => setSearch({ bucket: selectedBucket?.name ?? '', folder: key.replace(/\/$/, '') }))}
        onStorageChange={(id) => {
          requestNavigation(() => {
            setSelectedStorage(id);
            setSearch({});
          });
        }}
        onSelect={(bucket) => requestNavigation(() => setSearch({ bucket: bucket.name }))}
        onCreate={() => requestNavigation(() => {
          void navigate({ to: '/artifacts/create-bucket' });
        })}
        onEdit={(bucket) => requestNavigation(() => {
          void navigate({ to: '/artifacts/create-bucket', search: { bucket: bucket.name } });
        })}
        onPin={async (bucket) => {
          try {
            await mutations.pinBucket.mutateAsync({ name: bucket.name, isPinned: !bucket.isPinned });
          } catch (cause) {
            setActionError('Failed to update the bucket pin.');
            throw cause;
          }
        }}
        onDelete={async (bucket) => {
          try {
            await mutations.deleteBucket.mutateAsync(bucket.name);
            if (selectedBucket?.name === bucket.name) setSearch({});
          } catch (cause) {
            setActionError('Failed to delete the bucket.');
            throw cause;
          }
        }}
      />
      </Box>
      <Box sx={contentSx}>
        {(actionError ?? queryError ?? upload.error ?? zip.progress.error) !== undefined && (
          <Typography
            role="alert"
            sx={{ p: 2 }}
          >
            {actionError ?? queryError ?? upload.error ?? zip.progress.error}
          </Typography>
        )}
        {missingBucket ? (
          <Box sx={emptySx}>
            <Typography variant="headingSmall">{t('artifacts.page.bucketNotFound', 'Bucket not found')}</Typography>
            <Typography>
              {t(
                'artifacts.page.bucketNotFoundDescription',
                'The bucket “{{name}}” no longer exists or is unavailable.',
                { name: search.bucket ?? '' },
              )}
            </Typography>
            <Button onClick={() => setSearch({})}>
              {t('artifacts.page.chooseAnotherBucket', 'Choose another bucket')}
            </Button>
          </Box>
        ) : previewFile !== undefined && selectedBucket !== undefined && projectId !== undefined ? (
          <FilePreviewCanvas
            file={previewFile}
            projectId={projectId}
            bucket={selectedBucket.name}
            onClose={() => setSearch({ bucket: selectedBucket.name, folder: currentPrefix.replace(/\/$/, '') })}
            onDelete={(key) => mutations.deleteFile.mutateAsync({ bucket: selectedBucket.name, key })}
            onSaved={() => mutations.refreshFiles(selectedBucket.name)}
            onUnsavedChangesUpdate={setHasUnsavedChanges}
          />
        ) : selectedBucket !== undefined ? (
          <ArtifactTable
            bucket={selectedBucket.name}
            retentionDays={selectedBucket.retentionDays}
            contents={files.data ?? []}
            currentPrefix={currentPrefix}
            loading={files.isFetching}
            {...(files.isError ? { error: t('artifacts.page.loadFilesError', 'Failed to load artifacts.') } : {})}
            onPrefixChange={(prefix) => setSearch({ bucket: selectedBucket.name, folder: prefix.replace(/\/$/, '') })}
            onPreview={(item) => setSearch({ bucket: selectedBucket.name, file: item.key })}
            onDownload={(item) => {
              if (item.kind === 'folder') downloadItems([item]);
              else void downloadFile(item);
            }}
            onDownloadMany={downloadItems}
            onDelete={setPendingDelete}
            onUpload={upload.stageFiles}
          />
        ) : (
          <Box sx={emptySx}>
            <Typography variant="headingSmall">
              {t('artifacts.page.noBuckets', 'No buckets created yet')}
            </Typography>
            <Button
              variant="contained"
              startIcon={<AddOutlinedIcon />}
              onClick={() => void navigate({ to: '/artifacts/create-bucket' })}
            >
              {t('artifacts.page.createBucket', 'Create bucket')}
            </Button>
          </Box>
        )}
      </Box>
      <UploadPathDialog
        open={upload.pathDialogOpen}
        bucket={selectedBucket?.name ?? ''}
        currentPrefix={currentPrefix}
        onClose={upload.closePathDialog}
        onConfirm={upload.confirmPath}
      />
      <DuplicateResolutionDialog
        open={upload.duplicateDialogOpen}
        filenames={upload.duplicateFilenames}
        onCancel={upload.cancelDuplicates}
        onSkip={upload.skipDuplicates}
        onReplace={upload.replaceDuplicates}
        onKeepBoth={upload.keepBoth}
      />
      <ZipDownloadProgressDialog
        progress={zip.progress}
        onCancel={zip.cancel}
      />
      <Dialog
        open={pendingDelete.length > 0}
        onClose={() => setPendingDelete([])}
      >
        <DialogTitle>{t('artifacts.page.deleteTitle', 'Delete selected artifacts?')}</DialogTitle>
        <DialogContent>{t('artifacts.page.deleteDescription', 'This action cannot be undone.')}</DialogContent>
        <DialogActions>
          <Button onClick={() => setPendingDelete([])}>{t('common.cancel', 'Cancel')}</Button>
          <Button
            color="error"
            variant="contained"
            disabled={mutations.deleteMany.isPending}
            onClick={confirmDelete}
          >
            {t('common.delete', 'Delete')}
          </Button>
        </DialogActions>
      </Dialog>
      <Dialog
        open={pendingNavigation !== undefined}
        onClose={() => setPendingNavigation(undefined)}
      >
        <DialogTitle>{t('artifacts.page.unsavedTitle', 'Discard unsaved changes?')}</DialogTitle>
        <DialogContent>
          {t('artifacts.page.unsavedDescription', 'Your current file edits will be lost.')}
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setPendingNavigation(undefined)}>
            {t('artifacts.preview.keepEditing', 'Keep editing')}
          </Button>
          <Button
            color="warning"
            onClick={() => {
              const action = pendingNavigation;
              setPendingNavigation(undefined);
              setHasUnsavedChanges(false);
              action?.();
            }}
          >
            {t('common.discard', 'Discard')}
          </Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}

const rootSx: SxProps<Theme> = (theme) => ({
  height: '100%',
  display: 'flex',
  overflow: 'hidden',
  backgroundColor: theme.vars.palette.background.tabPanel,
});
/** `ARTIFACTS_PANEL_WIDTHS.DEFAULT_LEFT_PANEL` (300px) and the baseline's `3.75rem` collapsed rail (`Artifacts.jsx:809-817`) — measured 299px + a 1px rule on next.elitea.ai at 2000x1010. */
const sidebarSx = (collapsed: boolean): SxProps<Theme> => (theme) => ({
  width: collapsed ? '3.75rem' : '300px',
  flexShrink: 0,
  height: '100%',
  overflow: 'hidden',
  borderRight: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  backgroundColor: theme.vars.palette.background.default,
  transition: 'width 0.2s ease-in-out',
});
const contentSx: SxProps<Theme> = { flex: 1, minWidth: 0, height: '100%', overflow: 'hidden' };
const emptySx: SxProps<Theme> = (theme) => ({
  height: '100%',
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  justifyContent: 'center',
  gap: theme.spacing(2),
});

