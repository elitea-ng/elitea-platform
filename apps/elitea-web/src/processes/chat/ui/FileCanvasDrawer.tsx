/**
 * The FILE canvas's own drawer + error snackbar (issue #878) — split out of
 * `ChatWithEditors.tsx` purely to keep that file under the §3.5 400-line
 * budget and its own cyclomatic complexity under the oxlint ceiling; the
 * conditionals below (`session !== undefined`, `error !== undefined`) are
 * exactly the two branches that pushed it over.
 *
 * A second, INDEPENDENT drawer from the turn-canvas one `ChatWithEditors`
 * still mounts directly: this session has no `canvasId` (no `elitea_core`
 * canvas row), so it shares no socket room / presence / editor-mutex state
 * with that one — see `useFileCanvas.ts`'s own module doc.
 */
import type { ReactNode } from 'react';

import Drawer from '@mui/material/Drawer';
import Snackbar from '@mui/material/Snackbar';

import { CanvasEditor } from '@/features/chat-messages';

import type { UseFileCanvasResult } from './useFileCanvas';

export interface FileCanvasDrawerProps {
  readonly fileCanvas: UseFileCanvasResult;
  readonly projectId: string | undefined;
  readonly viewerId: string | undefined;
}

export function FileCanvasDrawer({ fileCanvas, projectId, viewerId }: FileCanvasDrawerProps): ReactNode {
  return (
    <>
      {fileCanvas.session !== undefined && projectId !== undefined && (
        <Drawer
          anchor="right"
          open
          onClose={fileCanvas.onCloseFileCanvas}
          slotProps={{ paper: { sx: { width: { xs: '100%', md: '48rem' }, maxWidth: '100%', p: 2, boxSizing: 'border-box' } } }}
          data-testid="chat-file-canvas-editor"
        >
          <CanvasEditor
            selectedCodeBlockInfo={{
              codeBlock: fileCanvas.session.codeBlock,
              language: fileCanvas.session.language,
              isBlock: true,
            }}
            onCloseCanvasEditor={fileCanvas.onCloseFileCanvas}
            projectId={projectId}
            {...(viewerId !== undefined ? { viewer: { id: viewerId } } : {})}
            saveToArtifacts={{ source: fileCanvas.session.source, onSaved: fileCanvas.onFileCanvasSaved }}
          />
        </Drawer>
      )}
      {fileCanvas.error !== undefined && (
        <Snackbar
          open
          autoHideDuration={6000}
          onClose={fileCanvas.onDismissFileCanvasError}
          message={fileCanvas.error}
          data-testid="chat-file-canvas-error"
        />
      )}
    </>
  );
}
