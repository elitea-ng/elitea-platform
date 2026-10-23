import type * as Pdfjs from 'pdfjs-dist';
// The worker is a same-origin file Vite copies out of the package, so it
// satisfies both CSPs as they are: the web's `worker-src 'self'` and the
// desktop's `script-src 'self'` (its CSP names no worker-src). `no-inline`,
// because a data: worker would be refused by both.
// oxlint-disable-next-line import/default -- a Vite `?url` import: the default export is the URL Vite adds, not the worker's own exports.
import pdfWorkerUrl from 'pdfjs-dist/build/pdf.worker.min.mjs?url&no-inline';

export type PdfjsModule = typeof Pdfjs;
export type PdfDocument = Pdfjs.PDFDocumentProxy;
export type PdfLoadingTask = Pdfjs.PDFDocumentLoadingTask;

let pdfjsModule: Promise<PdfjsModule> | undefined;

/**
 * pdf.js, imported the first time a PDF is opened so it stays out of the
 * route chunk. A failed import is forgotten, so the next PDF tries again.
 */
export function loadPdfjs(): Promise<PdfjsModule> {
  pdfjsModule ??= import('pdfjs-dist').then(
    (pdfjs) => {
      pdfjs.GlobalWorkerOptions.workerSrc = pdfWorkerUrl;
      return pdfjs;
    },
    (cause: unknown) => {
      pdfjsModule = undefined;
      throw cause;
    },
  );
  return pdfjsModule;
}

/**
 * Opens a PDF from its bytes, never from a URL: the desktop's `connect-src`
 * reaches only the host's IPC, and the bytes are already here. pdf.js 6 has
 * no eval path left (its `isEvalSupported` option is gone with it), so
 * neither CSP needs `unsafe-eval`.
 *
 * No CMap or standard-font URL is given, so fonts a PDF does not embed fall
 * back to system fonts. WebAssembly is off (neither CSP allows
 * `wasm-unsafe-eval`), so JPEG 2000 and JBIG2 images decode through pdf.js's
 * plain-JavaScript fallbacks, which it imports from `wasmUrl`: the build emits
 * them into a folder named for the pdf.js version beside the worker
 * (vite.config.ts, `pdfjsNoWasmDecoders`), and the dev
 * server serves them from the package's own `wasm/` folder.
 */
export function openPdfDocument(pdfjs: PdfjsModule, data: Uint8Array): PdfLoadingTask {
  return pdfjs.getDocument({
    data,
    useWasm: false,
    wasmUrl: noWasmDecodersUrl(pdfjs.version),
    enableXfa: false,
  });
}

/** Absolute, because the worker resolves it against its own URL. */
function noWasmDecodersUrl(version: string): string {
  const worker = new URL(pdfWorkerUrl, window.location.href);
  return new URL(import.meta.env.DEV ? '../wasm/' : `./pdfjs-${version}/`, worker).href;
}
