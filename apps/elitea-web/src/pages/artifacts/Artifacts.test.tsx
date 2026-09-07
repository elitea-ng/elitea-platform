import type { ReactNode } from 'react';

import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import * as artifactsFeature from '@/features/artifacts';
import * as sharedArtifacts from '@/shared/api/artifacts';
import * as runtimeConfig from '@/shared/config';
import * as download from '@/shared/lib/download';

const mocks = {
  buckets: {
    data: [{ id: 'bucket-1', name: 'docs', isPinned: false, createdAt: '2026-01-01T00:00:00Z' }],
    isFetching: false,
    isError: false,
  },
  files: {
    data: [{ key: 'readme.md', size: 12, lastModified: '2026-01-01T00:00:00Z', bucket: 'docs' }],
    isFetching: false,
    isError: false,
    refetch: vi.fn(),
  },
  storage: {
    data: [{ id: 'storage-1', title: 'Primary', shared: false }],
  },
  createBucket: vi.fn(),
  pinBucket: vi.fn(),
  deleteBucket: vi.fn(),
  deleteFile: vi.fn(),
  deleteMany: vi.fn(),
  stageFiles: vi.fn(),
  refreshFiles: vi.fn(),
  uploadOptions: undefined as { onUploaded: () => unknown } | undefined,
  fetchArtifactBlob: vi.fn(),
  triggerBlobDownload: vi.fn(),
  zipStart: vi.fn(),
  zipCancel: vi.fn(),
};

interface BucketSidebarMockProps {
  readonly onCreate: () => void;
  readonly onEdit: (bucket: { id: string; name: string; isPinned: boolean; createdAt: string }) => void;
  readonly onToggleCollapsed: () => void;
  readonly onSelectFile: (item: { key: string }) => void;
  readonly onSelectFolder: (key: string) => void;
  readonly onSelect: (bucket: { id: string; name: string; isPinned: boolean; createdAt: string }) => void;
  readonly onStorageChange: (id: string) => void;
  readonly onPin: (bucket: { id: string; name: string; isPinned: boolean; createdAt: string }) => Promise<unknown>;
  readonly onDelete: (bucket: { id: string; name: string; isPinned: boolean; createdAt: string }) => Promise<unknown>;
}

interface ArtifactTableMockProps {
  readonly error?: string;
  readonly onPrefixChange: (prefix: string) => void;
  readonly onPreview: (item: MockItem) => void;
  readonly onDownload: (item: MockItem) => void;
  readonly onDownloadMany: (items: readonly MockItem[]) => void;
  readonly onDelete: (items: readonly MockItem[]) => void;
  readonly onUpload: (files: readonly File[]) => void;
}

interface MockItem {
  readonly id: string;
  readonly key: string;
  readonly name: string;
  readonly kind: 'file' | 'folder';
  readonly size: number;
}

const bucket = { id: 'bucket-1', name: 'docs', isPinned: false, createdAt: '2026-01-01T00:00:00Z' };
const file: MockItem = { id: 'readme.md', key: 'readme.md', name: 'readme.md', kind: 'file', size: 12 };
const folder: MockItem = { id: 'folder/', key: 'folder/', name: 'folder', kind: 'folder', size: 0 };

function MockBucketSidebar(props: BucketSidebarMockProps): ReactNode {
  return (
    <nav>
      <button onClick={props.onCreate}>sidebar-create</button>
      <button onClick={() => props.onSelect(bucket)}>select-docs</button>
      <button onClick={() => props.onSelect({ ...bucket, id: 'bucket-2', name: 'reports' })}>select-reports</button>
      <button onClick={() => props.onStorageChange('storage-2')}>select-storage</button>
      <button onClick={() => void props.onPin(bucket).catch(() => undefined)}>pin-docs</button>
      <button onClick={() => void props.onDelete(bucket).catch(() => undefined)}>delete-docs</button>
      <button onClick={() => props.onEdit(bucket)}>edit-docs</button>
      <button onClick={props.onToggleCollapsed}>toggle-buckets</button>
      <button onClick={() => props.onSelectFile({ key: 'folder/deep.txt' })}>tree-select-file</button>
      <button onClick={() => props.onSelectFolder('folder/')}>tree-select-folder</button>
    </nav>
  );
}

function MockArtifactTable(props: ArtifactTableMockProps): ReactNode {
  return (
    <section>
      {props.error && <span>{props.error}</span>}
      <button onClick={() => props.onPrefixChange('folder/')}>open-folder</button>
      <button onClick={() => props.onPreview(file)}>preview-file</button>
      <button onClick={() => props.onDownload(file)}>download-file</button>
      <button onClick={() => props.onDownload(folder)}>download-folder</button>
      <button onClick={() => props.onDownloadMany([file, folder])}>download-many</button>
      <button onClick={() => props.onDelete([file])}>delete-file</button>
      <button onClick={() => props.onDelete([])}>delete-empty</button>
      <button onClick={() => props.onUpload([new File(['x'], 'x.txt')])}>upload-file</button>
    </section>
  );
}

function MockFilePreviewCanvas(props: {
  readonly onClose: () => void;
  readonly onDelete: (key: string) => Promise<unknown>;
  readonly onSaved: () => unknown;
  readonly onUnsavedChangesUpdate?: (hasChanges: boolean) => void;
}): ReactNode {
  return (
    <section>
      <span>file-preview</span>
      <button onClick={props.onClose}>close-preview</button>
      <button onClick={() => void props.onDelete('readme.md')}>preview-delete</button>
      <button onClick={() => void props.onSaved()}>preview-saved</button>
      <button onClick={() => props.onUnsavedChangesUpdate?.(true)}>mark-dirty</button>
    </section>
  );
}

import { Artifacts } from './Artifacts';
import { renderArtifactsRoute } from './__tests__/testRouter';

describe('Artifacts page', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    mocks.buckets.data = [bucket];
    mocks.buckets.isFetching = false;
    mocks.buckets.isError = false;
    mocks.files.data = [{ key: 'readme.md', size: 12, lastModified: '2026-01-01T00:00:00Z', bucket: 'docs' }];
    mocks.files.isFetching = false;
    mocks.files.isError = false;
    mocks.storage.data = [{ id: 'storage-1', title: 'Primary', shared: false }];
    mocks.fetchArtifactBlob.mockReset().mockResolvedValue({ ok: true, data: new Blob(['hello']) });
    for (const mutation of [
      mocks.pinBucket,
      mocks.deleteBucket,
      mocks.deleteFile,
      mocks.deleteMany,
    ]) {
      mutation.mockReset().mockResolvedValue(undefined);
    }
    mocks.stageFiles.mockReset();
    mocks.refreshFiles.mockReset().mockResolvedValue(undefined);
    mocks.triggerBlobDownload.mockReset();
    mocks.zipStart.mockReset().mockResolvedValue(undefined);
    mocks.files.refetch.mockReset().mockResolvedValue(undefined);
    vi.spyOn(artifactsFeature, 'BucketSidebar').mockImplementation(MockBucketSidebar as never);
    vi.spyOn(artifactsFeature, 'ArtifactTable').mockImplementation(MockArtifactTable as never);
    vi.spyOn(artifactsFeature, 'FilePreviewCanvas').mockImplementation(MockFilePreviewCanvas as never);
    vi.spyOn(artifactsFeature, 'UploadPathDialog').mockReturnValue(null);
    vi.spyOn(artifactsFeature, 'DuplicateResolutionDialog').mockReturnValue(null);
    vi.spyOn(artifactsFeature, 'ZipDownloadProgressDialog').mockReturnValue(null);
    vi.spyOn(artifactsFeature, 'expandFoldersToArtifactKeys').mockImplementation(
      ((items: readonly MockItem[]) =>
        items.flatMap((item) => item.kind === 'folder' ? ['folder/deep.txt'] : [item.key])),
    );
    vi.spyOn(artifactsFeature, 'useArtifactBuckets').mockReturnValue(mocks.buckets as never);
    vi.spyOn(artifactsFeature, 'useArtifacts').mockReturnValue(mocks.files as never);
    vi.spyOn(artifactsFeature, 'useArtifactStorageConfigurations').mockReturnValue(mocks.storage as never);
    vi.spyOn(artifactsFeature, 'useArtifactMutations').mockReturnValue({
      createBucket: { mutateAsync: mocks.createBucket, isPending: false },
      pinBucket: { mutateAsync: mocks.pinBucket },
      deleteBucket: { mutateAsync: mocks.deleteBucket },
      deleteFile: { mutateAsync: mocks.deleteFile },
      deleteMany: { mutateAsync: mocks.deleteMany, isPending: false },
      refreshFiles: mocks.refreshFiles,
    } as never);
    const uploadHookResult = {
      stageFiles: mocks.stageFiles,
      pathDialogOpen: false,
      closePathDialog: vi.fn(),
      confirmPath: vi.fn(),
      duplicateDialogOpen: false,
      duplicateFilenames: [],
      cancelDuplicates: vi.fn(),
      skipDuplicates: vi.fn(),
      replaceDuplicates: vi.fn(),
      keepBoth: vi.fn(),
    };
    vi.spyOn(artifactsFeature, 'useArtifactUpload').mockImplementation(((options: { onUploaded: () => unknown }) => {
      // Keep the real `onUploaded` callback so the test can fire it — that is
      // the only place the page decides WHICH queries a finished upload
      // refreshes, and mocking the hook away is what let it go stale.
      mocks.uploadOptions = options;
      return uploadHookResult;
    }) as never);
    vi.spyOn(artifactsFeature, 'useZipDownload').mockReturnValue({
      start: mocks.zipStart,
      cancel: mocks.zipCancel,
      progress: { phase: 'idle', completed: 0, total: 0 },
    } as never);
    vi.spyOn(sharedArtifacts, 'fetchArtifactBlob').mockImplementation(mocks.fetchArtifactBlob);
    vi.spyOn(runtimeConfig, 'getConfig').mockReturnValue({
      status: 'ok',
      config: {
        vite_server_url: '/api/v2',
        vite_base_uri: '/',
        vite_public_project_id: 'public',
        allow_project_own_llms: false,
      },
    });
    vi.spyOn(download, 'triggerBlobDownload').mockImplementation(mocks.triggerBlobDownload);
  });

  it('routes table navigation and stages uploads', async () => {
    const user = userEvent.setup();
    const { router } = renderArtifactsRoute(<Artifacts />, '/artifacts?bucket=docs');
    await user.click(await screen.findByRole('button', { name: 'open-folder' }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ bucket: 'docs', folder: 'folder' }));
    await user.click(screen.getByRole('button', { name: 'upload-file' }));
    expect(mocks.stageFiles).toHaveBeenCalledWith([expect.objectContaining({ name: 'x.txt' })]);
    await user.click(screen.getByRole('button', { name: 'select-storage' }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ bucket: 'docs', folder: '' }));
  });

  it('refreshes the BUCKET list after an upload, not only the object list', async () => {
    // The bucket panel's "Size:" footer is summed from the bucket list's
    // `size_bytes`. Refreshing only the object list left it showing the
    // pre-upload total until the next full page load — measured on the
    // standalone stack: 18 B after a second, 35 B upload; 53 B after F5.
    renderArtifactsRoute(<Artifacts />, '/artifacts?bucket=docs');
    expect(mocks.uploadOptions).toBeDefined();
    await mocks.uploadOptions?.onUploaded();
    expect(mocks.refreshFiles).toHaveBeenCalledWith('docs');
    expect(mocks.files.refetch).not.toHaveBeenCalled();
  });

  it('opens, closes, deletes, and refreshes a file preview', async () => {
    const user = userEvent.setup();
    const { router } = renderArtifactsRoute(<Artifacts />, '/artifacts?bucket=docs');
    await user.click(await screen.findByRole('button', { name: 'preview-file' }));
    expect(await screen.findByText('file-preview')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'preview-delete' }));
    expect(mocks.deleteFile).toHaveBeenCalledWith({ bucket: 'docs', key: 'readme.md' });
    await user.click(screen.getByRole('button', { name: 'preview-saved' }));
    // Both queries, not just the object list: an in-place edit changes the
    // object's byte length, and the buckets panel's "Size:" footer is summed
    // from the BUCKET list. See the upload case below.
    expect(mocks.refreshFiles).toHaveBeenCalledWith('docs');
    await user.click(screen.getByRole('button', { name: 'close-preview' }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ file: '' }));
  });

  it('guards bucket navigation when the preview has unsaved edits', async () => {
    const user = userEvent.setup();
    const { router } = renderArtifactsRoute(<Artifacts />, '/artifacts?bucket=docs&file=readme.md');
    await user.click(await screen.findByRole('button', { name: 'mark-dirty' }));
    await user.click(screen.getByRole('button', { name: 'select-reports' }));
    expect(screen.getByRole('dialog', { name: 'Discard unsaved changes?' })).toBeInTheDocument();
    expect(router.state.location.search).toMatchObject({ bucket: 'docs', file: 'readme.md' });
    await user.click(screen.getByRole('button', { name: 'Discard' }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ bucket: 'reports', file: '' }));
  });

  it('downloads individual files and groups folders into a ZIP', async () => {
    const user = userEvent.setup();
    renderArtifactsRoute(<Artifacts />, '/artifacts?bucket=docs');
    await user.click(await screen.findByRole('button', { name: 'download-file' }));
    await waitFor(() => expect(mocks.triggerBlobDownload).toHaveBeenCalledWith(expect.any(Blob), 'readme.md'));
    await user.click(screen.getByRole('button', { name: 'download-folder' }));
    expect(mocks.zipStart).toHaveBeenCalledWith(expect.objectContaining({
      projectId: 'project-1',
      bucket: 'docs',
      filenames: ['folder/deep.txt'],
    }));
    await user.click(screen.getByRole('button', { name: 'download-many' }));
    expect(mocks.zipStart).toHaveBeenCalledTimes(2);
  });

  it('pins and deletes buckets while preserving selection', async () => {
    const user = userEvent.setup();
    renderArtifactsRoute(<Artifacts />, '/artifacts?bucket=docs');
    await user.click(await screen.findByRole('button', { name: 'pin-docs' }));
    expect(mocks.pinBucket).toHaveBeenCalledWith({ name: 'docs', isPinned: true });
    await user.click(screen.getByRole('button', { name: 'delete-docs' }));
    expect(mocks.deleteBucket).toHaveBeenCalledWith('docs');
  });

  it('confirms artifact deletion and ignores empty expansions', async () => {
    const user = userEvent.setup();
    renderArtifactsRoute(<Artifacts />, '/artifacts?bucket=docs');
    await user.click(await screen.findByRole('button', { name: 'delete-file' }));
    await user.click(screen.getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(mocks.deleteMany).toHaveBeenCalledWith({
      bucket: 'docs',
      keys: ['readme.md'],
    }));
    await user.click(await screen.findByRole('button', { name: 'delete-empty' }));
    expect(mocks.deleteMany).toHaveBeenCalledTimes(1);
  });

  it('drives the bucket tree, the edit route and the collapse toggle', async () => {
    const user = userEvent.setup();
    const { router } = renderArtifactsRoute(<Artifacts />, '/artifacts?bucket=docs');
    await user.click(await screen.findByRole('button', { name: 'tree-select-file' }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ file: 'folder/deep.txt' }));
    await user.click(screen.getByRole('button', { name: 'tree-select-folder' }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ folder: 'folder' }));
    await user.click(screen.getByRole('button', { name: 'toggle-buckets' }));
    await user.click(screen.getByRole('button', { name: 'edit-docs' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/artifacts/create-bucket'));
  });

  it('surfaces a failed bucket pin', async () => {
    const user = userEvent.setup();
    mocks.pinBucket.mockRejectedValueOnce(new Error('nope'));
    renderArtifactsRoute(<Artifacts />, '/artifacts?bucket=docs');
    await user.click(await screen.findByRole('button', { name: 'pin-docs' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to update the bucket pin.');
  });

  it('surfaces a failed bucket delete', async () => {
    const user = userEvent.setup();
    mocks.deleteBucket.mockRejectedValueOnce(new Error('nope'));
    renderArtifactsRoute(<Artifacts />, '/artifacts?bucket=docs');
    await user.click(await screen.findByRole('button', { name: 'delete-docs' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to delete the bucket.');
  });

  it('surfaces a failed artifact delete', async () => {
    const user = userEvent.setup();
    mocks.deleteMany.mockRejectedValueOnce(new Error('nope'));
    renderArtifactsRoute(<Artifacts />, '/artifacts?bucket=docs');
    await user.click(await screen.findByRole('button', { name: 'delete-file' }));
    await user.click(screen.getByRole('button', { name: 'Delete' }));
    // `findByText`, not `findByRole('alert')`: a failed delete leaves the
    // confirmation dialog open, and an open MUI dialog puts `aria-hidden` on
    // everything behind it — including this message — so a role query cannot
    // reach it.
    expect(await screen.findByText('Failed to delete the selected artifacts.')).toBeInTheDocument();
  });

  it('keeps the file open when the unsaved-changes guard is dismissed', async () => {
    const user = userEvent.setup();
    const { router } = renderArtifactsRoute(<Artifacts />, '/artifacts?bucket=docs&file=readme.md');
    await user.click(await screen.findByRole('button', { name: 'mark-dirty' }));
    await user.click(screen.getByRole('button', { name: 'select-reports' }));
    await user.click(screen.getByRole('button', { name: 'Keep editing' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Discard unsaved changes?' })).toBeNull());
    expect(router.state.location.search).toMatchObject({ bucket: 'docs', file: 'readme.md' });
  });

  it('refuses to download when the runtime configuration is unavailable', async () => {
    const user = userEvent.setup();
    vi.spyOn(runtimeConfig, 'getConfig').mockReturnValue({ status: 'error' } as never);
    renderArtifactsRoute(<Artifacts />, '/artifacts?bucket=docs');
    await user.click(await screen.findByRole('button', { name: 'download-file' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Runtime configuration is unavailable.');
    expect(mocks.fetchArtifactBlob).not.toHaveBeenCalled();
  });

  it('reports a failed download without losing the table', async () => {
    const user = userEvent.setup();
    mocks.fetchArtifactBlob.mockResolvedValueOnce({ ok: false, error: { kind: 'network' } });
    renderArtifactsRoute(<Artifacts />, '/artifacts?bucket=docs');
    await user.click(await screen.findByRole('button', { name: 'download-file' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to download readme.md.');
  });

  it('shows empty and missing bucket states with working recovery actions', async () => {
    const user = userEvent.setup();
    mocks.buckets.data = [];
    const { router } = renderArtifactsRoute(<Artifacts />, '/artifacts');
    await user.click(await screen.findByRole('button', { name: 'Create bucket' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/artifacts/create-bucket'));
  });

  it('reports a missing selected bucket and returns to selection', async () => {
    const user = userEvent.setup();
    const { router } = renderArtifactsRoute(<Artifacts />, '/artifacts?bucket=missing');
    expect(await screen.findByText('Bucket not found')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Choose another bucket' }));
    await waitFor(() => expect(router.state.location.search).toMatchObject({ bucket: 'docs' }));
  });

  it('shows query errors without exposing internal details', async () => {
    mocks.buckets.isError = true;
    renderArtifactsRoute(<Artifacts />, '/artifacts?bucket=docs');
    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to load buckets.');
  });
});
