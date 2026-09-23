import { t } from '@/shared/i18n';

import { isMarkdownFile } from './isMarkdownFile';

/**
 * Project Context › Import: read one picked file into editor text, or say why
 * it was refused.
 *
 * The two refusals are the same shape on purpose — a toast the user can read —
 * because the alternative each replaced was silence:
 *
 *  - WRONG TYPE (#889 / ELITEA-0940): the handler used to read whatever `File`
 *    reached the hidden input and check only its LENGTH, so a `.txt` loaded
 *    into the editor exactly as Markdown would. The `accept` attribute binds
 *    the native dialog alone — see `isMarkdownFile`;
 *  - TOO LONG (issue 841): the reference toasts this
 *    (`ProjectContextEditor.jsx:127-146`); this port wrote to the console, so
 *    the import simply did not happen with nothing on screen to say so.
 *
 * Refusing NEVER touches the editor's existing content: a refused import that
 * had already cleared the buffer would be the same data loss with a toast on
 * top.
 */
export interface MarkdownImportHandlers {
  readonly maxChars: number;
  readonly onText: (text: string) => void;
  readonly onError: (message: string) => void;
}

export function readMarkdownImport(file: File, handlers: MarkdownImportHandlers): void {
  if (!isMarkdownFile(file)) {
    handlers.onError(
      t('entities.projectContext.content.fileWrongType', 'Only Markdown (.md) files can be imported'),
    );
    return;
  }
  const reader = new FileReader();
  reader.onload = (event) => {
    const result = (event.target?.result as string) ?? '';
    // CRLF and lone CR both normalised: the editor stores LF, and a file
    // authored on Windows must not count its line endings twice against the
    // character budget below.
    const text = String(result).replace(/\r\n/g, '\n').replace(/\r/g, '\n');
    if (text.length > handlers.maxChars) {
      handlers.onError(
        t('entities.projectContext.content.fileTooLarge', 'File content exceeds 2500 characters'),
      );
      return;
    }
    handlers.onText(text);
  };
  reader.readAsText(file);
}
