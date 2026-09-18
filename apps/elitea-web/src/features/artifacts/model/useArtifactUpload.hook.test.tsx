import { act, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { uploadArtifactObject } from '@/shared/api/artifacts';
import * as sharedArtifacts from '@/shared/api/artifacts';
import * as runtimeConfig from '@/shared/config';
import type { Artifact } from '@/entities/artifact';

import * as chatConfigApi from '../api/chatConfigApi';
import { renderHookWithProviders } from '../__tests__/testUtils';
import { useArtifactUpload } from './useArtifactUpload';

const contents: Artifact[] = [
  { key: 'existing.txt', size: 1, lastModified: '2026-01-01T00:00:00Z', bucket: 'docs' },
];

beforeEach(() => {
  vi.restoreAllMocks();
  vi.spyOn(sharedArtifacts, 'uploadArtifactObject');
  vi.spyOn(chatConfigApi, 'useChatConfig').mockReturnValue({
    data: { limits: { DEFAULT_MAX_FILE_SIZE: 100 } },
  });
  vi.spyOn(runtimeConfig, 'getConfig').mockReturnValue({
    status: 'ok',
    config: {
      vite_server_url: '/api/v2',
      vite_base_uri: '/',
      vite_public_project_id: 'public',
      allow_project_own_llms: false,
    },
  });
  vi.mocked(uploadArtifactObject).mockResolvedValue({
    ok: true,
    data: undefined,
    status: 200,
    headers: new Headers(),
  });
});

function renderUpload(contentsOverride: readonly Artifact[] = contents) {
  const onUploaded = vi.fn().mockResolvedValue(undefined);
  const hook = renderHookWithProviders(() => useArtifactUpload({
    projectId: 'p1',
    bucket: 'docs',
    contents: contentsOverride,
    currentPrefix: '',
    onUploaded,
  }));
  return { ...hook, onUploaded };
}

describe('useArtifactUpload', () => {
  it('stages a safe upload and refreshes after completion', async () => {
    const hook = renderUpload([]);
    const file = new File(['new'], 'new.txt', { type: 'text/plain' });
    act(() => hook.result.current.stageFiles([file]));
    expect(hook.result.current.pathDialogOpen).toBe(true);
    act(() => hook.result.current.confirmPath('folder'));
    await waitFor(() => expect(uploadArtifactObject).toHaveBeenCalledWith(expect.objectContaining({
      fileKey: 'folder/new.txt',
      projectId: 'p1',
    })));
    await waitFor(() => expect(hook.onUploaded).toHaveBeenCalled());
  });

  it('offers replace, skip, and keep-both duplicate strategies', async () => {
    const duplicate = new File(['x'], 'existing.txt');
    const hook = renderUpload();
    act(() => hook.result.current.stageFiles([duplicate]));
    act(() => hook.result.current.confirmPath(''));
    await waitFor(() => expect(hook.result.current.duplicateDialogOpen).toBe(true));
    act(() => hook.result.current.keepBoth());
    await waitFor(() => expect(uploadArtifactObject).toHaveBeenCalledWith(expect.objectContaining({
      fileKey: 'existing - Copy.txt',
    })));

    act(() => hook.result.current.stageFiles([duplicate]));
    act(() => hook.result.current.confirmPath(''));
    await waitFor(() => expect(hook.result.current.duplicateDialogOpen).toBe(true));
    act(() => hook.result.current.replaceDuplicates());
    await waitFor(() => expect(uploadArtifactObject).toHaveBeenCalledWith(expect.objectContaining({
      fileKey: 'existing.txt',
    })));

    act(() => hook.result.current.stageFiles([duplicate]));
    act(() => hook.result.current.confirmPath(''));
    await waitFor(() => expect(hook.result.current.duplicateDialogOpen).toBe(true));
    act(() => hook.result.current.skipDuplicates());
    await waitFor(() => expect(hook.onUploaded).toHaveBeenCalledTimes(3));
  });

  /* elitea_issues: #5355 — Skip in the duplicate dialog must drop only the duplicate file(s) and still upload the rest of the batch, not cancel the whole upload */
  it('Skip duplicates uploads the non-duplicate files from a mixed batch', async () => {
    const hook = renderUpload();
    const duplicate = new File(['x'], 'existing.txt');
    const fresh = new File(['y'], 'new.txt');
    act(() => hook.result.current.stageFiles([duplicate, fresh]));
    act(() => hook.result.current.confirmPath(''));
    await waitFor(() => expect(hook.result.current.duplicateDialogOpen).toBe(true));
    expect(hook.result.current.duplicateFilenames).toEqual(['existing.txt']);
    act(() => hook.result.current.skipDuplicates());
    await waitFor(() => expect(hook.onUploaded).toHaveBeenCalled());
    expect(uploadArtifactObject).toHaveBeenCalledWith(expect.objectContaining({ fileKey: 'new.txt' }));
    expect(uploadArtifactObject).not.toHaveBeenCalledWith(expect.objectContaining({ fileKey: 'existing.txt' }));
  });

  it('reports validation and transport failures', async () => {
    const hook = renderUpload([]);
    act(() => hook.result.current.stageFiles([new File(['bad'], 'bad#.txt')]));
    act(() => hook.result.current.confirmPath(''));
    await waitFor(() => expect(hook.result.current.error).toContain('bad#.txt'));
    vi.mocked(uploadArtifactObject).mockResolvedValue({
      ok: false,
      error: { kind: 'http', status: 500, url: '/upload', body: null },
    });
    act(() => hook.result.current.stageFiles([new File(['ok'], 'ok.txt')]));
    act(() => hook.result.current.confirmPath(''));
    await waitFor(() => expect(hook.result.current.error).toContain('Failed to upload'));
  });

  it('uploads a batch best-effort — one failing file does not stop the others from completing', async () => {
    const hook = renderUpload([]);
    vi.mocked(uploadArtifactObject).mockImplementation(({ fileKey }) =>
      Promise.resolve(
        fileKey.endsWith('bad.txt')
          ? { ok: false, error: { kind: 'http', status: 500, url: '/upload', body: null } }
          : { ok: true, data: undefined, status: 200, headers: new Headers() },
      ),
    );
    act(() => hook.result.current.stageFiles([new File(['x'], 'good.txt'), new File(['y'], 'bad.txt')]));
    act(() => hook.result.current.confirmPath(''));
    await waitFor(() => expect(hook.onUploaded).toHaveBeenCalledTimes(1));
    expect(hook.result.current.error).toContain('bad.txt');
    expect(hook.result.current.error).not.toContain('good.txt');
    expect(uploadArtifactObject).toHaveBeenCalledWith(expect.objectContaining({ fileKey: 'good.txt' }));
    expect(uploadArtifactObject).toHaveBeenCalledWith(expect.objectContaining({ fileKey: 'bad.txt' }));
  });
});
