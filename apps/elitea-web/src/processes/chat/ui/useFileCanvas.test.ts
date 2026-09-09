import { act, renderHook, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import * as chatMessagesFeature from '@/features/chat-messages';

import { useFileCanvas } from './useFileCanvas';

beforeEach(() => {
  vi.restoreAllMocks();
  vi.spyOn(chatMessagesFeature, 'openArtifactFileInCanvas');
});

describe('useFileCanvas', () => {
  it('opens a session from a fetched file, and lets a save update its source', async () => {
    vi.mocked(chatMessagesFeature.openArtifactFileInCanvas).mockResolvedValue({
      ok: true,
      file: { codeBlock: 'print(1)', language: 'python', type: 'code', source: { bucket: 'docs', name: 'script.py' } },
    });
    const { result } = renderHook(() => useFileCanvas('p1'));

    act(() => result.current.onOpenFileInCanvas({ bucket: 'docs', name: 'script.py' }));
    await waitFor(() => expect(result.current.session).toBeDefined());
    expect(result.current.session).toEqual({
      codeBlock: 'print(1)',
      language: 'python',
      source: { bucket: 'docs', name: 'script.py' },
    });
    expect(chatMessagesFeature.openArtifactFileInCanvas).toHaveBeenCalledWith({ projectId: 'p1', bucket: 'docs', name: 'script.py' });

    act(() => result.current.onFileCanvasSaved({ bucket: 'reports', name: 'script.py' }));
    expect(result.current.session?.source).toEqual({ bucket: 'reports', name: 'script.py' });

    act(() => result.current.onCloseFileCanvas());
    expect(result.current.session).toBeUndefined();
  });

  it('reports why a file could not be opened, without touching the session', async () => {
    vi.mocked(chatMessagesFeature.openArtifactFileInCanvas).mockResolvedValue({ ok: false, reason: 'too-large' });
    const { result } = renderHook(() => useFileCanvas('p1'));

    act(() => result.current.onOpenFileInCanvas({ bucket: 'docs', name: 'huge.py' }));
    await waitFor(() => expect(result.current.error).toBeDefined());
    expect(result.current.session).toBeUndefined();

    act(() => result.current.onDismissFileCanvasError());
    expect(result.current.error).toBeUndefined();
  });

  it('does nothing with no project selected', () => {
    const { result } = renderHook(() => useFileCanvas(undefined));
    act(() => result.current.onOpenFileInCanvas({ bucket: 'docs', name: 'script.py' }));
    expect(chatMessagesFeature.openArtifactFileInCanvas).not.toHaveBeenCalled();
  });
});
