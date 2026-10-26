import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { renderWithProviders } from '../__tests__/testUtils';
import { loadPdfjs, openPdfDocument, type PdfjsModule, type PdfLoadingTask } from '../lib/pdfjs';
import * as pdfjsLoader from '../lib/pdfjs';
import { ArtifactPreviewContent } from './ArtifactPreviewContent';
import { PdfPreview } from './PdfPreview';

interface FakeTask {
  readonly promise: Promise<unknown>;
  readonly destroy: ReturnType<typeof vi.fn>;
}

const renderCancel = vi.fn();
const getPage = vi.fn((pageNumber: number) => Promise.resolve({
  pageNumber,
  getViewport: ({ scale }: { scale: number }) => ({ width: 600 * scale, height: 800 * scale }),
  render: () => ({ promise: Promise.resolve(), cancel: renderCancel }),
}));

function fakeTask(document: Promise<unknown>): FakeTask {
  // A rejection the component has not awaited yet must not fail the run.
  document.catch(() => undefined);
  return { promise: document, destroy: vi.fn().mockResolvedValue(undefined) };
}

function pdfBlob(): Blob {
  return new Blob(['%PDF-1.7'], { type: 'application/pdf' });
}

beforeEach(() => {
  vi.restoreAllMocks();
  vi.clearAllMocks();
  // pdf.js itself stays out of jsdom: the loader is the seam, as the
  // network is for the other preview tests.
  vi.spyOn(pdfjsLoader, 'loadPdfjs').mockResolvedValue({} as PdfjsModule);
  vi.spyOn(pdfjsLoader, 'openPdfDocument');
});

describe('PdfPreview', () => {
  it('shows a loading state, then the page count, and pages through the document', async () => {
    let resolveDocument: (value: unknown) => void = () => undefined;
    const task = fakeTask(new Promise((resolve) => {
      resolveDocument = resolve;
    }));
    vi.mocked(openPdfDocument).mockReturnValue(task as unknown as PdfLoadingTask);
    const user = userEvent.setup();
    renderWithProviders(
      <PdfPreview
        data={pdfBlob()}
        filename="report.pdf"
        onDownload={vi.fn()}
      />,
    );
    expect(screen.getByLabelText('Loading PDF')).toBeInTheDocument();
    await waitFor(() => expect(openPdfDocument).toHaveBeenCalled());
    const bytes = vi.mocked(openPdfDocument).mock.calls[0]?.[1];
    expect(bytes).toBeInstanceOf(Uint8Array);
    expect(new TextDecoder().decode(bytes)).toBe('%PDF-1.7');

    await act(() => {
      resolveDocument({ numPages: 3, getPage });
      return Promise.resolve();
    });
    expect(await screen.findByTestId('artifacts-pdf-page-indicator')).toHaveTextContent('Page 1 of 3');
    expect(screen.getByTestId('artifacts-pdf-canvas').tagName).toBe('CANVAS');
    await waitFor(() => expect(getPage).toHaveBeenCalledWith(1));
    expect(screen.getByRole('button', { name: 'Previous page' })).toBeDisabled();

    await user.click(screen.getByRole('button', { name: 'Next page' }));
    expect(screen.getByTestId('artifacts-pdf-page-indicator')).toHaveTextContent('Page 2 of 3');
    await waitFor(() => expect(getPage).toHaveBeenCalledWith(2));
    // The page-1 render is cancelled when the page changes.
    expect(renderCancel).toHaveBeenCalled();
  });

  it('shows an error with a working download for a PDF pdf.js cannot open', async () => {
    const passwordError = Object.assign(new Error('No password given'), { name: 'PasswordException' });
    vi.mocked(openPdfDocument).mockReturnValue(fakeTask(Promise.reject(passwordError)) as unknown as PdfLoadingTask);
    const onDownload = vi.fn();
    const user = userEvent.setup();
    renderWithProviders(
      <PdfPreview
        data={pdfBlob()}
        filename="locked.pdf"
        onDownload={onDownload}
      />,
    );
    expect(await screen.findByRole('alert')).toHaveTextContent('encrypted or damaged');
    await user.click(screen.getByRole('button', { name: 'Download file' }));
    expect(onDownload).toHaveBeenCalled();
  });

  it('shows a page that fails to render as an error in its place, and keeps the others', async () => {
    const renderPage = vi.fn((pageNumber: number) => ({
      promise: pageNumber === 2 ? Promise.reject(new Error('bad page')) : Promise.resolve(),
      cancel: renderCancel,
    }));
    const pages = vi.fn((pageNumber: number) => Promise.resolve({
      pageNumber,
      getViewport: ({ scale }: { scale: number }) => ({ width: 600 * scale, height: 800 * scale }),
      render: () => renderPage(pageNumber),
    }));
    vi.mocked(openPdfDocument).mockReturnValue(
      fakeTask(Promise.resolve({ numPages: 3, getPage: pages })) as unknown as PdfLoadingTask,
    );
    const onDownload = vi.fn();
    const user = userEvent.setup();
    renderWithProviders(
      <PdfPreview
        data={pdfBlob()}
        filename="report.pdf"
        onDownload={onDownload}
      />,
    );
    expect(await screen.findByTestId('artifacts-pdf-page-indicator')).toHaveTextContent('Page 1 of 3');
    await waitFor(() => expect(renderPage).toHaveBeenCalledWith(1));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Next page' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Page 2 cannot be shown');
    // The way on stays, and the download works from the page's error.
    expect(screen.getByTestId('artifacts-pdf-page-indicator')).toHaveTextContent('Page 2 of 3');
    await user.click(screen.getByRole('button', { name: 'Download file' }));
    expect(onDownload).toHaveBeenCalled();

    await user.click(screen.getByRole('button', { name: 'Next page' }));
    await waitFor(() => expect(renderPage).toHaveBeenCalledWith(3));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByTestId('artifacts-pdf-canvas')).toBeVisible();
  });

  it('shows the error when pdf.js itself fails to load', async () => {
    vi.mocked(loadPdfjs).mockRejectedValue(new Error('chunk failed'));
    renderWithProviders(
      <PdfPreview
        data={pdfBlob()}
        filename="report.pdf"
        onDownload={vi.fn()}
      />,
    );
    expect(await screen.findByRole('alert')).toHaveTextContent('cannot be shown');
    expect(openPdfDocument).not.toHaveBeenCalled();
  });

  it('destroys the loading task on unmount and when the file changes', async () => {
    const first = fakeTask(Promise.resolve({ numPages: 1, getPage }));
    const second = fakeTask(Promise.resolve({ numPages: 2, getPage }));
    vi.mocked(openPdfDocument)
      .mockReturnValueOnce(first as unknown as PdfLoadingTask)
      .mockReturnValueOnce(second as unknown as PdfLoadingTask);
    const { rerender, unmount } = renderWithProviders(
      <PdfPreview
        data={pdfBlob()}
        filename="one.pdf"
        onDownload={vi.fn()}
      />,
    );
    expect(await screen.findByTestId('artifacts-pdf-page-indicator')).toHaveTextContent('Page 1 of 1');
    rerender(
      <PdfPreview
        data={pdfBlob()}
        filename="two.pdf"
        onDownload={vi.fn()}
      />,
    );
    expect(first.destroy).toHaveBeenCalled();
    expect(await screen.findByTestId('artifacts-pdf-page-indicator')).toHaveTextContent('Page 1 of 2');
    unmount();
    expect(second.destroy).toHaveBeenCalled();
  });
});

describe('ArtifactPreviewContent PDF branch', () => {
  const baseProps = {
    filename: 'report.pdf',
    content: '',
    mode: 'rendered' as const,
    onChange: vi.fn(),
    onDownload: vi.fn(),
  };

  it('renders the PDF from its bytes, and a message without them', async () => {
    vi.mocked(openPdfDocument).mockReturnValue(
      fakeTask(Promise.resolve({ numPages: 4, getPage })) as unknown as PdfLoadingTask,
    );
    const { rerender } = renderWithProviders(
      <ArtifactPreviewContent
        {...baseProps}
        kind="pdf"
      />,
    );
    expect(screen.getByText('No PDF to display.')).toBeInTheDocument();
    rerender(
      <ArtifactPreviewContent
        {...baseProps}
        kind="pdf"
        pdfData={pdfBlob()}
      />,
    );
    expect(await screen.findByTestId('artifacts-pdf-page-indicator')).toHaveTextContent('Page 1 of 4');
    expect(screen.queryByLabelText('Edit report.pdf')).not.toBeInTheDocument();
  });

  it('keeps the size gate for PDFs', () => {
    renderWithProviders(
      <ArtifactPreviewContent
        {...baseProps}
        kind="pdf"
        isOversized
        pdfData={pdfBlob()}
      />,
    );
    expect(screen.getByText(/too large to preview/)).toBeInTheDocument();
    expect(openPdfDocument).not.toHaveBeenCalled();
  });
});
