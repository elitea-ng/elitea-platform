/**
 * "Open in canvas" from the bucket browser (issue #878) — split out of
 * `Artifacts.tsx` purely to keep that file under the §3.5 400-line budget;
 * the state, the fetch, and the drawer that PAINTS it all moved together so
 * the page itself only renders one node and reads one error string.
 *
 * No `canvasId` is ever passed to `CanvasEditor`: this session has no
 * `elitea_core` canvas row, so its socket room / presence roster both stay
 * dormant (both are gated on `selectedCodeBlockInfo?.canvasId`) — the
 * artifact object itself is this canvas's only server-side identity.
 */
import { useCallback, useState } from 'react';

import Drawer from '@mui/material/Drawer';

import { CanvasEditor, openArtifactFileInCanvas } from '@/features/chat-messages';
import type { ArtifactListItem } from '@/features/artifacts';
import { t } from '@/shared/i18n';

interface CanvasSession {
  readonly codeBlock: string;
  readonly language: string;
  readonly source: { readonly bucket: string; readonly name: string };
}

export interface UseArtifactCanvasResult {
  readonly openInCanvas: (item: ArtifactListItem) => void;
  readonly error: string | undefined;
  /** The drawer itself — render it once, unconditionally, in the page. */
  readonly node: React.ReactNode;
}

export function useArtifactCanvas(
  projectId: string | undefined,
  bucketName: string | undefined,
  refreshFiles: (bucket: string) => unknown,
): UseArtifactCanvasResult {
  const [session, setSession] = useState<CanvasSession>();
  const [error, setError] = useState<string>();

  const openInCanvas = useCallback(
    (item: ArtifactListItem) => {
      if (projectId === undefined || bucketName === undefined) return;
      setError(undefined);
      void openArtifactFileInCanvas({ projectId, bucket: bucketName, name: item.key, size: item.size }).then((result) => {
        if (!result.ok) {
          setError(
            result.reason === 'too-large'
              ? t('artifacts.canvas.tooLarge', '{{name}} is too large to open in canvas.', { name: item.name })
              : t('artifacts.canvas.unsupportedOrFailed', 'Could not open {{name}} in canvas.', { name: item.name }),
          );
          return;
        }
        setSession({ codeBlock: result.file.codeBlock, language: result.file.language, source: result.file.source });
      });
    },
    [projectId, bucketName],
  );

  const node =
    session !== undefined && projectId !== undefined ? (
      <Drawer
        anchor="right"
        open
        onClose={() => setSession(undefined)}
        slotProps={{ paper: { sx: { width: { xs: '100%', md: '48rem' }, maxWidth: '100%', p: 2, boxSizing: 'border-box' } } }}
        data-testid="artifacts-canvas-editor"
      >
        <CanvasEditor
          selectedCodeBlockInfo={{ codeBlock: session.codeBlock, language: session.language, isBlock: true }}
          onCloseCanvasEditor={() => setSession(undefined)}
          projectId={projectId}
          saveToArtifacts={{
            source: session.source,
            onSaved: (source) => {
              setSession((current) => (current ? { ...current, source } : current));
              void refreshFiles(source.bucket);
            },
          }}
        />
      </Drawer>
    ) : null;

  return { openInCanvas, error, node };
}
