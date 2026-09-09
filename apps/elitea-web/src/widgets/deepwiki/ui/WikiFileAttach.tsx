/**
 * Attach small text files to the next question (#873).
 *
 * A SECOND attachment kind alongside `WikiContextPicker`'s wiki pages: files
 * the reader picks from their own machine rather than from the wiki's own
 * manifest. Where a wiki page is an IDENTIFIER the server resolves, a file
 * here is CONTENT the browser reads locally with `FileReader` and sends as
 * `WikiChatTarget.attachments` — there is nothing on the server for the
 * client to name, so there is no identifier to send in the first place (see
 * `run/extracontext.go`'s header for the full "content, not identifier"
 * reasoning).
 *
 * VALIDATED BEFORE A BYTE IS READ. `validateWikiAttachmentFiles`
 * (`../lib/attachmentValidation.ts`) enforces the count cap, the size cap and
 * the extension allowlist against the FILE, not its content — a picker that
 * read first and complained after would spend the reader's time on a file
 * that was always going to be refused.
 */
import { memo, useCallback, useRef, useState, type ChangeEvent } from 'react';

import Alert from '@mui/material/Alert';
import AttachFileIcon from '@mui/icons-material/AttachFile';
import Badge from '@mui/material/Badge';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import Snackbar from '@mui/material/Snackbar';
import Stack from '@mui/material/Stack';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';

import { validateWikiAttachmentFiles } from '../lib/attachmentValidation';

/** One attached file, read into memory. */
export interface WikiFileAttachment {
  readonly name: string;
  readonly content: string;
}

export interface WikiFileAttachProps {
  readonly attachments: readonly WikiFileAttachment[];
  readonly onChange: (attachments: readonly WikiFileAttachment[]) => void;
  readonly disabled: boolean;
}

/** Reads one File's text. Rejects on a read failure rather than attaching an empty string that reads as "this file had nothing in it". */
function readAsText(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      resolve(typeof reader.result === 'string' ? reader.result : '');
    };
    reader.onerror = () => {
      reject(reader.error ?? new Error(`could not read ${file.name}`));
    };
    reader.readAsText(file);
  });
}

export const WikiFileAttach = memo(function WikiFileAttach({
  attachments,
  onChange,
  disabled,
}: WikiFileAttachProps) {
  const inputRef = useRef<HTMLInputElement>(null);
  const [error, setError] = useState<string | null>(null);

  const openPicker = useCallback(() => {
    inputRef.current?.click();
  }, []);

  const remove = useCallback(
    (name: string) => {
      onChange(attachments.filter((attachment) => attachment.name !== name));
    },
    [attachments, onChange],
  );

  const onFilesPicked = useCallback(
    (event: ChangeEvent<HTMLInputElement>) => {
      const picked = Array.from(event.target.files ?? []);
      // Cleared immediately so picking the SAME file again after removing it
      // still fires a change event — the browser does not fire one for an
      // unchanged file list otherwise.
      event.target.value = '';
      if (picked.length === 0) return;

      const { validFiles, errors } = validateWikiAttachmentFiles(picked, attachments);
      setError(errors[0] ?? null);
      if (validFiles.length === 0) return;

      void Promise.all(validFiles.map(async (file) => ({ name: file.name, content: await readAsText(file) })))
        .then((read) => {
          onChange([...attachments, ...read]);
        })
        .catch(() => {
          setError(t('widgets.deepwiki.chat.attachReadFailed', 'One of the attached files could not be read.'));
        });
    },
    [attachments, onChange],
  );

  const label = t('widgets.deepwiki.chat.attachFiles', 'Attach files');

  return (
    <>
      <input
        ref={inputRef}
        type="file"
        multiple
        hidden
        onChange={onFilesPicked}
        data-testid="wiki-chat-attach-input"
      />
      <Tooltip title={label}>
        <span>
          <IconButton
            size="small"
            onClick={openPicker}
            disabled={disabled}
            aria-label={label}
            data-testid="wiki-chat-attach-button"
          >
            <Badge badgeContent={attachments.length} color="primary">
              <AttachFileIcon fontSize="small" />
            </Badge>
          </IconButton>
        </span>
      </Tooltip>

      {attachments.length > 0 ? (
        <Stack
          sx={{ flexDirection: 'row', flexWrap: 'wrap', gap: 0.5, width: '100%' }}
          data-testid="wiki-chat-attach-chips"
        >
          {attachments.map((attachment) => (
            <Chip
              key={attachment.name}
              size="small"
              label={attachment.name}
              data-testid="wiki-chat-attach-chip"
              onDelete={
                disabled
                  ? undefined
                  : () => {
                      remove(attachment.name);
                    }
              }
            />
          ))}
        </Stack>
      ) : null}

      <Snackbar
        open={error !== null}
        autoHideDuration={6000}
        onClose={() => {
          setError(null);
        }}
      >
        <Alert severity="warning" onClose={() => { setError(null); }} data-testid="wiki-chat-attach-error">
          {error}
        </Alert>
      </Snackbar>
    </>
  );
});
