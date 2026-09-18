/**
 * Project Context › Import accepts MARKDOWN, and this is what enforces it
 * (#889 / ELITEA-0940).
 *
 * The hidden `<input type="file" accept=".md,text/markdown">` the toolbar
 * clicks is a HINT to the native file dialog and nothing more. Switching that
 * dialog to "All files", dragging a file onto the input, or driving it from
 * automation all put an arbitrary `File` on it, and `handleFileUpload` used to
 * read whatever arrived and check only its LENGTH — so a `.txt` (or a `.pdf`,
 * or a binary) loaded into the editor exactly as a Markdown file would, with
 * no error at all.
 */

/**
 * BOTH the extension and the MIME type are checked, and either is enough,
 * because neither is reliable on its own:
 *
 *  - the browser reports `text/markdown` for a `.md` file on some platforms
 *    and `text/plain` or `''` on others — the type comes from the OS registry,
 *    not from the bytes;
 *  - a file dragged out of an archive, or a `File` built by a script, can
 *    carry a correct type and a name with no extension at all.
 *
 * Refusing a genuine `.md` because the OS declined to name its type would be a
 * worse failure than the one being fixed, so a match on either side admits it.
 */
const MARKDOWN_FILE_EXTENSIONS = ['.md', '.markdown', '.mdown', '.mkd'] as const;
const MARKDOWN_FILE_TYPES = ['text/markdown', 'text/x-markdown'] as const;

/** Whether this file is importable as the project's Markdown context. */
export function isMarkdownFile(file: File): boolean {
  const name = file.name.toLowerCase();
  if (MARKDOWN_FILE_EXTENSIONS.some((extension) => name.endsWith(extension))) return true;
  return MARKDOWN_FILE_TYPES.includes(file.type.toLowerCase() as (typeof MARKDOWN_FILE_TYPES)[number]);
}
