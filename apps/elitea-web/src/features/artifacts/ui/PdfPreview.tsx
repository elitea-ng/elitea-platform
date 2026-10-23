import type { ReactNode } from 'react';
import { useEffect, useRef, useState } from 'react';

import NavigateBeforeIcon from '@mui/icons-material/NavigateBefore';
import NavigateNextIcon from '@mui/icons-material/NavigateNext';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import IconButton from '@mui/material/IconButton';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { loadPdfjs, openPdfDocument, type PdfDocument, type PdfLoadingTask } from '../lib/pdfjs';
import { useElementWidth } from '../lib/useElementWidth';

interface PdfPreviewProps {
  readonly data: Blob;
  readonly filename: string;
  readonly onDownload: () => void;
}

/**
 * WebKit (the desktop's WKWebView, Safari) refuses a canvas over 16 777 216
 * pixels and draws nothing; the render is scaled down to stay under it.
 */
const MAX_CANVAS_PIXELS = 16_777_216;

function isRenderCancelled(cause: unknown): boolean {
  return typeof cause === 'object' && cause !== null
    && (cause as { name?: unknown }).name === 'RenderingCancelledException';
}

/** The opened document, or why there is none. */
interface PdfDocumentState {
  readonly pdf?: PdfDocument;
  readonly failed: boolean;
}

function usePdfDocument(data: Blob): PdfDocumentState {
  const [state, setState] = useState<PdfDocumentState>({ failed: false });

  useEffect(() => {
    setState({ failed: false });
    let active = true;
    let task: PdfLoadingTask | undefined;
    void (async () => {
      try {
        const [pdfjs, bytes] = await Promise.all([loadPdfjs(), data.arrayBuffer()]);
        if (!active) return;
        task = openPdfDocument(pdfjs, new Uint8Array(bytes));
        const pdf = await task.promise;
        if (active) setState({ pdf, failed: false });
      } catch {
        // An encrypted PDF (pdf.js asks for a password nobody can give here),
        // a damaged one, or pdf.js that would not load: the same answer.
        if (active) setState({ failed: true });
      }
    })();
    return () => {
      active = false;
      // Destroying the loading task destroys its document and ends its worker.
      void task?.destroy();
    };
  }, [data]);

  return state;
}

function PdfFailure({ onDownload }: { readonly onDownload: () => void }): ReactNode {
  return (
    <Box sx={{ display: 'grid', placeItems: 'center', minHeight: '16rem', textAlign: 'center' }}>
      <Box>
        <Typography variant="headingSmall">
          {t('artifacts.preview.unavailableTitle', 'Preview unavailable')}
        </Typography>
        <Typography
          role="alert"
          variant="bodySmall"
          sx={{ mt: 1, mb: 2 }}
        >
          {t(
            'artifacts.preview.pdfFailed',
            'This PDF cannot be shown. It may be encrypted or damaged. Download it to open it.',
          )}
        </Typography>
        <Button
          variant="contained"
          onClick={onDownload}
        >
          {t('artifacts.preview.downloadFile', 'Download file')}
        </Button>
      </Box>
    </Box>
  );
}

/** One page at a time, scaled to the pane's width. Mounted once per opened document. */
function PdfPages(props: { readonly pdf: PdfDocument; readonly filename: string; readonly onDownload: () => void }): ReactNode {
  const { pdf } = props;
  const [page, setPage] = useState(1);
  const [renderFailed, setRenderFailed] = useState(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const width = Math.max(1, Math.floor(useElementWidth(containerRef, 800)));
  const pageCount = pdf.numPages;

  useEffect(() => {
    const canvas = canvasRef.current;
    if (canvas === null) return;
    let active = true;
    let cancelRender: (() => void) | undefined;
    void (async () => {
      try {
        const pdfPage = await pdf.getPage(page);
        if (!active) return;
        const natural = pdfPage.getViewport({ scale: 1 });
        const ratio = window.devicePixelRatio || 1;
        const fit = width / natural.width;
        const pixels = natural.width * natural.height * (fit * ratio) ** 2;
        const scale = fit * ratio * Math.min(1, Math.sqrt(MAX_CANVAS_PIXELS / pixels));
        const viewport = pdfPage.getViewport({ scale });
        canvas.width = Math.floor(viewport.width);
        canvas.height = Math.floor(viewport.height);
        canvas.style.width = `${Math.floor(natural.width * fit)}px`;
        const task = pdfPage.render({ canvas, viewport });
        cancelRender = () => task.cancel();
        await task.promise;
      } catch (cause) {
        if (active && !isRenderCancelled(cause)) setRenderFailed(true);
      }
    })();
    return () => {
      active = false;
      cancelRender?.();
    };
  }, [pdf, page, width]);

  if (renderFailed) return <PdfFailure onDownload={props.onDownload} />;

  return (
    <Box
      ref={containerRef}
      sx={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: 1, minWidth: 0 }}
    >
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
        <IconButton
          size="small"
          aria-label={t('artifacts.preview.pdfPreviousPage', 'Previous page')}
          disabled={page <= 1}
          onClick={() => setPage((current) => Math.max(1, current - 1))}
        >
          <NavigateBeforeIcon />
        </IconButton>
        <Typography
          variant="bodySmall"
          data-testid="artifacts-pdf-page-indicator"
        >
          {t('artifacts.preview.pdfPageIndicator', 'Page {{page}} of {{total}}', { page, total: pageCount })}
        </Typography>
        <IconButton
          size="small"
          aria-label={t('artifacts.preview.pdfNextPage', 'Next page')}
          disabled={page >= pageCount}
          onClick={() => setPage((current) => Math.min(pageCount, current + 1))}
        >
          <NavigateNextIcon />
        </IconButton>
      </Box>
      <Box
        component="canvas"
        ref={canvasRef}
        aria-label={t('artifacts.preview.pdfPageAria', 'Page {{page}} of {{name}}', { page, name: props.filename })}
        data-testid="artifacts-pdf-canvas"
        sx={{ display: 'block', maxWidth: '100%', height: 'auto' }}
      />
    </Box>
  );
}

/**
 * A PDF drawn by pdf.js onto a canvas: no iframe, object or embed, which the
 * web CSP refuses (`frame-src 'none'`, `object-src 'none'`), and no text or
 * annotation layer, so a link in the PDF is not clickable here.
 */
export function PdfPreview(props: PdfPreviewProps): ReactNode {
  const { pdf, failed } = usePdfDocument(props.data);
  if (failed) return <PdfFailure onDownload={props.onDownload} />;
  if (pdf === undefined) {
    return <CircularProgress aria-label={t('artifacts.preview.pdfLoadingAria', 'Loading PDF')} />;
  }
  return (
    <PdfPages
      pdf={pdf}
      filename={props.filename}
      onDownload={props.onDownload}
    />
  );
}
