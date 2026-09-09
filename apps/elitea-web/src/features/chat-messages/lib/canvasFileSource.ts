/**
 * lib/canvasFileSource.ts — turning a stored FILE (an artifact bucket object,
 * or a chat attachment backed by one) into something the canvas editor can
 * open, and back again on save.
 *
 * Issue #878: the canvas editor could edit a code/diagram/table block carved
 * out of the CURRENT turn and could not open a previously-stored file, nor
 * save its own document back into the artifact store as a named, browsable
 * object. This module is the pure half of that gap — kind detection by
 * filename, the size cap, and the `source` reference a file-backed canvas
 * carries so a later Save updates the SAME object rather than creating a
 * sibling.
 *
 * Kind mapping deviates from a literal "markdown -> table" reading on
 * purpose: a markdown FILE is not necessarily a single GFM table (the canvas
 * `table` pane parses `markdownTable` syntax specifically, and forcing an
 * arbitrary `.md` file through it would silently mangle prose). `.csv`/
 * `.tsv` DO map to `table` — that pane already imports delimited data
 * (`./canvas/table/ImportTableButton.tsx`) — and `.md`/`.markdown`/`.txt`/
 * `.text` map to `document` (issue #879): a prose file opens into the
 * rich-text canvas rather than a plain code pane, the same distinction the
 * "Open as document" answer action and the selection-to-canvas affordance
 * make for prose carved out of the CURRENT turn.
 *
 * The extension lists mirror (deliberately duplicated, not imported)
 * `features/artifacts/lib/artifactParsers.ts`'s `artifactPreviewKind` — this
 * feature does not import a sibling feature (`no-sideways-features`,
 * `.dependency-cruiser.cjs`; `chat-messages` carries only a TEMPORARY,
 * pre-existing waiver for that rule, recorded as such in the waiver's own
 * commit message, not a standing invitation to add new sideways imports).
 *
 * ── `.docx` (issue #879) ───────────────────────────────────────────────────
 * No docx-to-text converter is in this app's bundle (checked: no `mammoth`,
 * no `docx`, nothing under `features/artifacts` either) and the spec for
 * this issue is explicit that one is added ONLY if it is already there. So
 * `.docx` (and the other binary office formats already refused by the size/
 * kind checks above it) is named explicitly rather than falling through the
 * generic "unrecognised extension" branch, so a caller can show "this format
 * isn't supported — download it instead" rather than the generic
 * open-failed message a truly unknown extension gets.
 */

/** The four kinds `Canvas`/`CanvasEditor` render. `document` is issue #879's addition — prose, edited as rich text, round-tripped as Markdown. */
export type CanvasFileOpenKind = 'code' | 'diagram' | 'table' | 'document';

/** What opening a file in canvas needs to know before it fetches anything. */
export interface CanvasFileOpenPlan {
  readonly type: CanvasFileOpenKind;
  /** The CodeMirror/canvas language id — `'markdownTable'` for a table, `'mermaid'` for a diagram, the file's own language otherwise. */
  readonly language: string;
}

const DIAGRAM_EXTENSIONS = new Set(['mmd', 'mermaid']);
const TABLE_EXTENSIONS = new Set(['csv', 'tsv']);

/** Extension -> CodeMirror language id, mirroring `./canvas/canvasLanguageOptions.ts`'s value set for the common cases; unmapped extensions fall back to the extension itself, which `getCanvasCodeExtensions` already treats as "no highlighting". */
const CODE_LANGUAGE_BY_EXTENSION: ReadonlyMap<string, string> = new Map([
  ['md', 'markdown'], ['markdown', 'markdown'],
  ['js', 'javascript'], ['jsx', 'javascript'], ['mjs', 'javascript'], ['cjs', 'javascript'],
  ['ts', 'typescript'], ['tsx', 'typescript'],
  ['py', 'python'],
  ['json', 'json'], ['jsonl', 'json'], ['ndjson', 'json'],
  ['yml', 'yaml'], ['yaml', 'yaml'],
  ['sh', 'shell'], ['bash', 'shell'], ['zsh', 'shell'],
  ['sql', 'sql'],
  ['html', 'html'], ['htm', 'html'],
  ['css', 'css'], ['scss', 'css'], ['less', 'css'],
  ['go', 'go'],
  ['rs', 'rust'],
  ['java', 'java'],
  ['c', 'c'], ['h', 'c'],
  ['cpp', 'cpp'], ['cc', 'cpp'], ['hpp', 'cpp'],
  ['xml', 'xml'],
]);

/** Extensions that open as a `document` (issue #879) — prose, not a fenced code block. */
const DOCUMENT_EXTENSIONS = new Set(['md', 'markdown', 'txt', 'text']);

/** A file this app can meaningfully preview as text — not exhaustive, but every extension a text editor is asked to open. Binary/office formats (images, docx, …) are refused rather than guessed at. */
const OPENABLE_TEXT_EXTENSIONS = new Set<string>([
  ...CODE_LANGUAGE_BY_EXTENSION.keys(),
  ...DIAGRAM_EXTENSIONS,
  ...TABLE_EXTENSIONS,
  ...DOCUMENT_EXTENSIONS,
  'log', 'ini', 'conf', 'toml', 'env', 'gitignore', 'dockerfile', 'makefile',
]);

/**
 * Office/binary formats this app recognises by name but refuses to open —
 * distinct from an extension it has simply never heard of. `.docx` is the
 * one issue #879 names explicitly; the rest are named for the same reason
 * `features/artifacts/lib/artifactParsers.ts` already refuses them as a
 * text preview (no converter for any of them is in this bundle).
 */
const UNSUPPORTED_BINARY_DOCUMENT_EXTENSIONS = new Set([
  'docx', 'doc', 'odt', 'rtf', 'pdf', 'xlsx', 'xls', 'pptx', 'ppt',
]);

/** Whether `filename` names a recognised-but-unopenable office format (issue #879: `.docx` and siblings) — distinct from an extension this app has never heard of, so the caller can say "unsupported, download instead" rather than a generic open-failed message. */
export function isUnsupportedCanvasDocumentFormat(filename: string): boolean {
  return UNSUPPORTED_BINARY_DOCUMENT_EXTENSIONS.has(extensionOf(filename));
}

function extensionOf(filename: string): string {
  const lower = filename.toLowerCase();
  const basename = lower.split('/').pop() ?? lower;
  return basename.includes('.') ? (basename.split('.').pop() ?? '') : basename;
}

/**
 * What opening `filename` in canvas would look like, or `undefined` when the
 * file is not a text-like kind canvas can render (binary, an office format,
 * or an extension this app does not recognise at all).
 */
export function detectCanvasFileOpenKind(filename: string): CanvasFileOpenPlan | undefined {
  const extension = extensionOf(filename);
  if (DIAGRAM_EXTENSIONS.has(extension)) return { type: 'diagram', language: 'mermaid' };
  if (TABLE_EXTENSIONS.has(extension)) return { type: 'table', language: 'markdownTable' };
  if (DOCUMENT_EXTENSIONS.has(extension)) return { type: 'document', language: 'document' };
  if (!OPENABLE_TEXT_EXTENSIONS.has(extension)) return undefined;
  return { type: 'code', language: CODE_LANGUAGE_BY_EXTENSION.get(extension) ?? extension };
}

/** Mirrors `features/artifacts/lib/artifactParsers.ts`'s `ARTIFACT_PREVIEW_SIZE_LIMIT_BYTES` — the same 2 MiB cap, applied here to "is this file small enough to load into an in-browser text editor" rather than "is this file small enough to preview". */
export const CANVAS_FILE_OPEN_SIZE_LIMIT_BYTES = 2 * 1024 * 1024;

export function isCanvasFileOpenSizeOk(sizeBytes: number | undefined): boolean {
  return typeof sizeBytes !== 'number' || sizeBytes <= CANVAS_FILE_OPEN_SIZE_LIMIT_BYTES;
}

/**
 * The reference a file-backed canvas keeps so Save can write back to the SAME
 * object instead of creating a sibling. `etag` is carried when the caller had
 * one (a bucket-listing read, or a download response's own `ETag` header) —
 * purely informational today (no If-Match precondition is sent), but keeping
 * it on the reference is what lets a future save refuse a conflicting
 * overwrite without a second round trip to re-discover it.
 */
export interface CanvasFileSource {
  readonly bucket: string;
  readonly name: string;
  readonly etag?: string;
}
