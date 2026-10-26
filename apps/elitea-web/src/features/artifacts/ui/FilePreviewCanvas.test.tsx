import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { fetchArtifactBlob, uploadArtifactObject } from '@/shared/api/artifacts';
import * as sharedArtifacts from '@/shared/api/artifacts';
import { getConfig } from '@/shared/config';
import * as runtimeConfig from '@/shared/config';
import { triggerBlobDownload } from '@/shared/lib/download';
import * as download from '@/shared/lib/download';

import { renderWithProviders } from '../__tests__/testUtils';
import type { ArtifactListItem } from '../model/types';
import * as previewContent from './ArtifactPreviewContent';
import { FilePreviewCanvas } from './FilePreviewCanvas';

function MockArtifactPreviewContent({
  kind,
  content,
  mode,
  imageUrl,
  pdfData,
  onChange,
  onDownload,
}: {
  readonly kind: string;
  readonly content: string;
  readonly mode: string;
  readonly imageUrl?: string;
  readonly pdfData?: Blob;
  readonly onChange: (value: string) => void;
  readonly onDownload: () => void;
}) {
  return (
    <div>
      <span>{kind}:{mode}</span>
      {imageUrl !== undefined && <span>image:{imageUrl}</span>}
      {pdfData !== undefined && <span>pdf:{pdfData.type}</span>}
      <textarea
        aria-label="preview editor"
        value={content}
        onChange={(event) => onChange(event.target.value)}
      />
      <button
        type="button"
        onClick={onDownload}
      >
        Child download
      </button>
    </div>
  );
}

const file: ArtifactListItem = {
  id: 'notes.txt',
  key: 'folder/notes.txt',
  name: 'notes.txt',
  kind: 'file',
  size: 5,
};
const textBlob = { text: vi.fn().mockResolvedValue('hello') } as unknown as Blob;
const okBlob = { ok: true, data: textBlob, status: 200, headers: new Headers() } as const;

beforeEach(() => {
  vi.restoreAllMocks();
  vi.spyOn(sharedArtifacts, 'fetchArtifactBlob');
  vi.spyOn(sharedArtifacts, 'uploadArtifactObject');
  vi.spyOn(runtimeConfig, 'getConfig');
  vi.spyOn(download, 'triggerBlobDownload').mockImplementation(() => undefined);
  vi.spyOn(previewContent, 'ArtifactPreviewContent').mockImplementation(MockArtifactPreviewContent as never);
  vi.mocked(getConfig).mockReturnValue({
    status: 'ok',
    config: {
      vite_server_url: '/api/v2',
      vite_base_uri: '/',
      vite_public_project_id: 'public',
      allow_project_own_llms: false,
    },
  });
  vi.mocked(fetchArtifactBlob).mockResolvedValue(okBlob);
  vi.mocked(uploadArtifactObject).mockResolvedValue({
    ok: true,
    data: undefined,
    status: 200,
    headers: new Headers(),
  });
});

function renderPreview(overrides: Partial<Parameters<typeof FilePreviewCanvas>[0]> = {}) {
  const props = {
    file,
    projectId: 'p1',
    bucket: 'docs',
    onClose: vi.fn(),
    onDelete: vi.fn().mockResolvedValue(undefined),
    onSaved: vi.fn().mockResolvedValue(undefined),
    onUnsavedChangesUpdate: vi.fn(),
    ...overrides,
  };
  renderWithProviders(<FilePreviewCanvas {...props} />);
  return props;
}

describe('FilePreviewCanvas', () => {
  it('loads, edits, saves, downloads, and closes text files', async () => {
    const user = userEvent.setup();
    const props = renderPreview();
    const editor = await screen.findByRole('textbox', { name: 'preview editor' });
    expect(editor).toHaveValue('hello');
    await user.clear(editor);
    await user.type(editor, 'updated');
    expect(props.onUnsavedChangesUpdate).toHaveBeenLastCalledWith(true);
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(uploadArtifactObject).toHaveBeenCalledWith(expect.objectContaining({
      fileKey: 'folder/notes.txt',
      projectId: 'p1',
    })));
    expect(props.onSaved).toHaveBeenCalled();
    await user.click(screen.getByRole('button', { name: 'Download file' }));
    await waitFor(() => expect(triggerBlobDownload).toHaveBeenCalledWith(textBlob, 'notes.txt'));
    await user.click(screen.getByRole('button', { name: 'Close preview' }));
    expect(props.onClose).toHaveBeenCalled();
  });

  it('protects unsaved edits and deletes after confirmation', async () => {
    const user = userEvent.setup();
    const props = renderPreview();
    const editor = await screen.findByRole('textbox', { name: 'preview editor' });
    await user.type(editor, ' changed');
    await user.click(screen.getByRole('button', { name: 'Close preview' }));
    expect(screen.getByRole('dialog', { name: 'Discard unsaved changes?' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Keep editing' }));
    await waitFor(() => {
      expect(screen.queryByRole('dialog', { name: 'Discard unsaved changes?' })).not.toBeInTheDocument();
    });
    await user.click(screen.getByRole('button', { name: 'Delete file' }));
    await user.click(screen.getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(props.onDelete).toHaveBeenCalledWith('folder/notes.txt'));
    expect(props.onClose).toHaveBeenCalled();
  });

  it('supports render-mode switching, discard, and download from the fallback content', async () => {
    const user = userEvent.setup();
    const props = renderPreview({
      file: { ...file, id: 'readme.md', key: 'readme.md', name: 'readme.md' },
    });
    await screen.findByText('markdown:rendered');
    await user.click(screen.getByRole('button', { name: 'Code' }));
    expect(screen.getByText('markdown:code')).toBeInTheDocument();
    const editor = screen.getByRole('textbox', { name: 'preview editor' });
    await user.type(editor, ' changed');
    await user.click(screen.getByRole('button', { name: 'Discard' }));
    expect(editor).toHaveValue('hello');
    await user.click(screen.getByRole('button', { name: 'Child download' }));
    await waitFor(() => expect(triggerBlobDownload).toHaveBeenCalled());
    expect(props.onClose).not.toHaveBeenCalled();
  });

  it('renders download-only files without fetching content', () => {
    renderPreview({
      file: { ...file, id: 'report.docx', key: 'report.docx', name: 'report.docx' },
    });
    expect(screen.getByText('docx:code')).toBeInTheDocument();
    expect(fetchArtifactBlob).not.toHaveBeenCalled();
  });

  it('surfaces load, save, download, and runtime configuration errors', async () => {
    vi.mocked(fetchArtifactBlob).mockResolvedValueOnce({
      ok: false,
      error: { kind: 'http', status: 500, url: '/file', body: null },
    });
    const user = userEvent.setup();
    renderPreview();
    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to load');

    vi.mocked(fetchArtifactBlob).mockResolvedValueOnce({
      ok: false,
      error: { kind: 'http', status: 500, url: '/file', body: null },
    });
    await user.click(screen.getByRole('button', { name: 'Download file' }));
    expect(screen.getByRole('alert')).toHaveTextContent('Failed to download');

    vi.mocked(getConfig).mockReturnValueOnce({ status: 'missing', missing: ['vite_server_url'], reasons: {} });
    await user.click(screen.getByRole('button', { name: 'Download file' }));
    expect(screen.getByRole('alert')).toHaveTextContent('Runtime configuration');
  });

  it('previews a PDF read-only from its fetched bytes', async () => {
    const pdf = new Blob(['%PDF-1.7'], { type: 'application/pdf' });
    vi.mocked(fetchArtifactBlob).mockResolvedValue({ ok: true, data: pdf, status: 200, headers: new Headers() });
    renderPreview({ file: { ...file, id: 'report.pdf', key: 'docs/report.pdf', name: 'report.pdf' } });
    expect(await screen.findByText('pdf:application/pdf')).toBeInTheDocument();
    expect(fetchArtifactBlob).toHaveBeenCalledWith(expect.objectContaining({ filePath: 'docs/report.pdf' }));
    expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Code' })).not.toBeInTheDocument();
  });

  it('does not fetch a PDF over the size limit', () => {
    renderPreview({ file: { ...file, id: 'big.pdf', key: 'big.pdf', name: 'big.pdf', size: 3 * 1024 * 1024 } });
    expect(screen.getByText('pdf:rendered')).toBeInTheDocument();
    expect(fetchArtifactBlob).not.toHaveBeenCalled();
  });

  it('shows an untyped SVG as an image/svg+xml object URL, and revokes it on unmount', async () => {
    const svg = new Blob(['<svg xmlns="http://www.w3.org/2000/svg"/>'], { type: 'application/octet-stream' });
    vi.mocked(fetchArtifactBlob).mockResolvedValue({ ok: true, data: svg, status: 200, headers: new Headers() });
    const createObjectURL = vi.fn((_blob: Blob) => 'blob:svg-preview');
    const revokeObjectURL = vi.fn();
    vi.stubGlobal('URL', Object.assign(URL, { createObjectURL, revokeObjectURL }));
    const { unmount } = renderWithProviders(
      <FilePreviewCanvas
        file={{ ...file, id: 'logo.svg', key: 'logo.svg', name: 'logo.svg' }}
        projectId="p1"
        bucket="docs"
        onClose={vi.fn()}
        onDelete={vi.fn()}
        onSaved={vi.fn()}
      />,
    );
    expect(await screen.findByText('image:blob:svg-preview')).toBeInTheDocument();
    expect(createObjectURL.mock.calls[0]?.[0].type).toBe('image/svg+xml');
    expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();
    unmount();
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:svg-preview');
    vi.unstubAllGlobals();
  });

  it('keeps the preview open when deletion fails', async () => {
    const user = userEvent.setup();
    const props = renderPreview({
      onDelete: vi.fn().mockRejectedValue(new Error('delete failure')),
    });
    await screen.findByRole('textbox', { name: 'preview editor' });
    await user.click(screen.getByRole('button', { name: 'Delete file' }));
    await user.click(screen.getByRole('button', { name: 'Delete' }));
    expect(await screen.findByText('Failed to delete file.')).toBeInTheDocument();
    expect(props.onClose).not.toHaveBeenCalled();
  });
});
