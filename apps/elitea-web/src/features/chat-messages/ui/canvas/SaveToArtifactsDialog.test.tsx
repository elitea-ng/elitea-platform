import { ThemeProvider } from '@mui/material/styles';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import * as canvasFileTransfer from '../../model/canvasFileTransfer';
import { SaveToArtifactsDialog } from './SaveToArtifactsDialog';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function renderDialog(overrides: Partial<Parameters<typeof SaveToArtifactsDialog>[0]> = {}) {
  const props = {
    open: true,
    projectId: 'p1',
    content: 'document body',
    onClose: vi.fn(),
    onSaved: vi.fn(),
    ...overrides,
  };
  render(
    <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
      <SaveToArtifactsDialog {...props} />
    </ThemeProvider>,
  );
  return props;
}

beforeEach(() => {
  vi.restoreAllMocks();
  vi.spyOn(canvasFileTransfer, 'listArtifactBucketNames').mockResolvedValue(['docs', 'reports']);
  vi.spyOn(canvasFileTransfer, 'artifactObjectExists').mockResolvedValue(false);
  vi.spyOn(canvasFileTransfer, 'saveCanvasToArtifact').mockResolvedValue({ ok: true });
});

describe('SaveToArtifactsDialog', () => {
  it('prefills the bucket and filename from an existing source, and saves without asking to overwrite (same object)', async () => {
    const user = userEvent.setup();
    const props = renderDialog({ source: { bucket: 'docs', name: 'notes.md' } });

    await waitFor(() => expect(screen.getByTestId('canvas-save-filename-input')).toHaveValue('notes.md'));
    await user.click(screen.getByTestId('canvas-save-submit'));

    await waitFor(() => {
      expect(canvasFileTransfer.saveCanvasToArtifact).toHaveBeenCalledWith({
        projectId: 'p1',
        bucket: 'docs',
        name: 'notes.md',
        content: 'document body',
      });
    });
    // Saving the SAME object the canvas was opened from must not stop for an
    // overwrite confirmation — that IS the save, not a surprising collision.
    expect(canvasFileTransfer.artifactObjectExists).not.toHaveBeenCalled();
    expect(props.onSaved).toHaveBeenCalledWith({ bucket: 'docs', name: 'notes.md' });
  });

  it('suggests a filename with no prior source, lets the bucket be chosen, and confirms before overwriting a DIFFERENT existing file', async () => {
    const user = userEvent.setup();
    vi.mocked(canvasFileTransfer.artifactObjectExists).mockResolvedValue(true);
    const props = renderDialog({ suggestedName: 'Edit code.py' });

    await waitFor(() => expect(screen.getByTestId('canvas-save-bucket-select')).toHaveValue('docs'));
    expect(screen.getByTestId('canvas-save-filename-input')).toHaveValue('Edit code.py');

    await user.click(screen.getByTestId('canvas-save-submit'));

    await screen.findByTestId('canvas-save-overwrite-warning');
    expect(canvasFileTransfer.saveCanvasToArtifact).not.toHaveBeenCalled();

    await user.click(screen.getByTestId('canvas-save-confirm-overwrite'));
    await waitFor(() => {
      expect(canvasFileTransfer.saveCanvasToArtifact).toHaveBeenCalledWith({
        projectId: 'p1',
        bucket: 'docs',
        name: 'Edit code.py',
        content: 'document body',
      });
    });
    expect(props.onSaved).toHaveBeenCalledWith({ bucket: 'docs', name: 'Edit code.py' });
  });

  it('surfaces a failed upload without closing', async () => {
    const user = userEvent.setup();
    vi.mocked(canvasFileTransfer.saveCanvasToArtifact).mockResolvedValue({ ok: false, reason: 'upload-failed' });
    const props = renderDialog({ source: { bucket: 'docs', name: 'notes.md' } });

    await waitFor(() => expect(screen.getByTestId('canvas-save-filename-input')).toHaveValue('notes.md'));
    await user.click(screen.getByTestId('canvas-save-submit'));

    await screen.findByRole('alert');
    expect(props.onSaved).not.toHaveBeenCalled();
    expect(props.onClose).not.toHaveBeenCalled();
  });
});
