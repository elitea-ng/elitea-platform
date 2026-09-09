/**
 * "Open in canvas" from a message ATTACHMENT (issue #878) — the composition
 * point `NormalAttachment`'s new control has nothing wired to until this
 * hook. Kept entirely separate from `useCanvasEditing`/`useEditorMutex`'s
 * turn-canvas drawer, deliberately: an attachment-opened canvas has no
 * `elitea_core` canvas row (no `canvasId`), so it needs none of that
 * machinery's socket room / presence / editor-mutex-queue concerns — its
 * only server-side identity is the artifact object itself, exactly like
 * `pages/artifacts/Artifacts.tsx`'s own "Open in canvas" session, which this
 * mirrors on purpose (same shape, different trigger).
 */
import { useCallback, useState } from 'react';

import { openArtifactFileInCanvas } from '@/features/chat-messages';
import type { CanvasFileSource } from '@/features/chat-messages';
import { t } from '@/shared/i18n';

interface FileCanvasSession {
  readonly codeBlock: string;
  readonly language: string;
  readonly source: CanvasFileSource;
}

export interface UseFileCanvasResult {
  readonly session: FileCanvasSession | undefined;
  readonly error: string | undefined;
  /** Fetches `source` and opens it — the attachment card's own click handler. */
  readonly onOpenFileInCanvas: (source: { readonly bucket: string; readonly name: string }) => void;
  readonly onCloseFileCanvas: () => void;
  /** Updates the open session's `source` after a save — so a plain re-save writes the SAME object rather than asking again. */
  readonly onFileCanvasSaved: (source: CanvasFileSource) => void;
  readonly onDismissFileCanvasError: () => void;
}

export function useFileCanvas(projectId: string | undefined): UseFileCanvasResult {
  const [session, setSession] = useState<FileCanvasSession>();
  const [error, setError] = useState<string>();

  const open = useCallback(
    (source: { readonly bucket: string; readonly name: string }) => {
      if (projectId === undefined) return;
      setError(undefined);
      void openArtifactFileInCanvas({ projectId, bucket: source.bucket, name: source.name }).then((result) => {
        if (!result.ok) {
          setError(
            result.reason === 'too-large'
              ? t('processes.chat.fileCanvas.tooLarge', 'This file is too large to open in canvas.')
              : result.reason === 'unsupported-format'
                ? t(
                    'processes.chat.fileCanvas.unsupportedFormat',
                    'This file format is not supported in canvas — download it instead.',
                  )
                : t('processes.chat.fileCanvas.openFailed', 'Could not open this file in canvas.'),
          );
          return;
        }
        setSession({ codeBlock: result.file.codeBlock, language: result.file.language, source: result.file.source });
      });
    },
    [projectId],
  );

  const close = useCallback(() => setSession(undefined), []);

  const onSaved = useCallback((source: CanvasFileSource) => {
    setSession((current) => (current ? { ...current, source } : current));
  }, []);

  const dismissError = useCallback(() => setError(undefined), []);

  return {
    session,
    error,
    onOpenFileInCanvas: open,
    onCloseFileCanvas: close,
    onFileCanvasSaved: onSaved,
    onDismissFileCanvasError: dismissError,
  };
}
