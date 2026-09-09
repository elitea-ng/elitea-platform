/**
 * MessageInput — the assistant's message box.
 *
 * # ATTACHMENTS (issue #625 item 2, extended by #877)
 *
 * #625 landed ONE file at a time, picked through a plain
 * `<input type="file">`, with no drag-and-drop, no clipboard paste, and no
 * upload-progress chip — real capability gaps against the published
 * widget, kept out deliberately at the time to stay a small, provably-
 * working control.
 *
 * #877 closes those gaps:
 *  - Multi-file selection (`<input multiple>`), validated against the SAME
 *    `ATTACHMENT_LIMITS` the main chat composer's `AttachmentButton` uses
 *    (`@/shared/lib/attachments` — count/size/per-image-size caps), not a
 *    second set of numbers invented for this widget.
 *  - Drag-and-drop onto the input area — `input.css`'s
 *    `elitea-assistant-input-area--drag-over`/`elitea-assistant-drop-overlay`
 *    classes existed, unused, before this change (left in place by an
 *    earlier port of the vendored theme).
 *  - Clipboard paste on the textarea (images and any other pasted file).
 *  - Per-file status chips (queued / uploading / done / error) with a
 *    remove button, using `input.css`'s own `--completed`/`--error`/
 *    `file-chip-spinner` classes, likewise pre-existing and unused.
 *
 * There is no byte-level upload-progress SIGNAL available here (the upload
 * goes through `eliteaFetch`, not the XHR-based `shared/api/upload.ts` the
 * main chat composer's progress bar reads from) — the spinner is
 * deliberately indeterminate rather than a fabricated percentage. See
 * `useChat.handleSend` (`../../lib/hooks/chat.hook.ts`) for the actual
 * per-file upload sequencing this component's status callback reflects.
 *
 * A picked/dropped/pasted file DOES reach the agent: `useChat`'s
 * `handleSend` uploads every attached file (`adapter.api.ts`'s
 * `uploadAttachment`, called once per file — no batch route exists) before
 * starting the turn, and includes the resulting filepaths in `startTurn`'s
 * `attachments` field. Support's own `Predict` route
 * (`internal/api/v2/supportassistant/predict.go`) reads that field the
 * same way the main chat composer's `payload.attachments` does.
 *
 * Every attachment is cleared the moment `onSend` settles, successfully or
 * not — same as the composer text. A failed upload is reported by
 * `useChat`'s own error message in the transcript, not by this component
 * holding the file for a manual retry.
 */
import type { ChangeEvent, ClipboardEvent, DragEvent, KeyboardEvent } from 'react';
import { memo, useCallback, useMemo, useRef, useState } from 'react';

import { AttachmentIcon, CheckIcon, CloseIcon, ErrorIcon, FileIcon, SendIcon, SpinnerIcon } from '../icons';

import { ATTACHMENT_LIMITS, validateAttachmentFiles } from '@/shared/lib/attachments';
import { t } from '@/shared/i18n';

export type TAttachmentStatus = 'pending' | 'uploading' | 'done' | 'error';

interface TAttachmentEntry {
  readonly id: string;
  readonly file: File;
  readonly status: TAttachmentStatus;
}

/** Called once per file as `useChat.handleSend` uploads it, in order. */
export type TAttachmentStatusCallback = (file: File, status: TAttachmentStatus) => void;

type TMessageInputProps = {
  placeholder: string;
  text: string;
  onTextChange: (text: string) => void;
  onSend: (text: string, files?: readonly File[], onFileStatus?: TAttachmentStatusCallback) => void | Promise<void>;
  disabled?: boolean | undefined;
};

function newAttachmentId(): string {
  return typeof crypto.randomUUID === 'function' ? crypto.randomUUID() : `attachment-${Date.now()}-${Math.random()}`;
}

const MessageInput = memo((props: TMessageInputProps) => {
  const { placeholder, text, onTextChange, onSend, disabled } = props;
  const [attachments, setAttachments] = useState<readonly TAttachmentEntry[]>([]);
  const [isSending, setIsSending] = useState(false);
  const [isDragOver, setIsDragOver] = useState(false);
  const [validationError, setValidationError] = useState<string | undefined>(undefined);
  const fileInputRef = useRef<HTMLInputElement>(null);

  // Attaching a file does not waive the message: `content` stays required
  // (SupportPredictPayload.content is min_length=1 server-side), so a file
  // with no words still cannot be sent.
  const isSendDisabled = useMemo(
    () => Boolean(disabled) || isSending || text.trim() === '',
    [disabled, isSending, text],
  );

  const addFiles = useCallback(
    (incoming: readonly File[]) => {
      if (incoming.length === 0) return;
      const { validFiles, errors } = validateAttachmentFiles(
        incoming,
        attachments.map((entry) => entry.file),
        ATTACHMENT_LIMITS,
      );
      setValidationError(errors.length > 0 ? errors.join(' ') : undefined);
      if (validFiles.length > 0) {
        setAttachments((prev) => [
          ...prev,
          ...validFiles.map((file) => ({ id: newAttachmentId(), file, status: 'pending' as const })),
        ]);
      }
    },
    [attachments],
  );

  const handleSend = useCallback(() => {
    const trimmed = text.trim();
    if (!trimmed || isSendDisabled) return;
    onTextChange('');
    const files = attachments.map((entry) => entry.file);
    setIsSending(true);
    const onFileStatus: TAttachmentStatusCallback = (file, status) => {
      setAttachments((prev) => prev.map((entry) => (entry.file === file ? { ...entry, status } : entry)));
    };
    void Promise.resolve(onSend(trimmed, files.length > 0 ? files : undefined, files.length > 0 ? onFileStatus : undefined)).finally(
      () => {
        setIsSending(false);
        setAttachments([]);
        setValidationError(undefined);
      },
    );
  }, [text, isSendDisabled, onTextChange, onSend, attachments]);

  const handleKeyDown = useCallback(
    (event: KeyboardEvent<HTMLTextAreaElement>) => {
      // Enter sends; Shift+Enter is a newline — the reference's binding.
      if (event.key === 'Enter' && !event.shiftKey) {
        event.preventDefault();
        handleSend();
      }
    },
    [handleSend],
  );

  const handlePaste = useCallback(
    (event: ClipboardEvent<HTMLTextAreaElement>) => {
      const items = event.clipboardData.items;
      if (!items) return;
      const files: File[] = [];
      for (const item of items) {
        if (item.kind === 'file') {
          const file = item.getAsFile();
          if (file) files.push(file);
        }
      }
      if (files.length > 0) {
        // Files, not text: keep the pasted bytes out of the textarea.
        event.preventDefault();
        addFiles(files);
      }
      // Otherwise fall through to the default text-paste behaviour.
    },
    [addFiles],
  );

  const handleFileSelected = useCallback(
    (event: ChangeEvent<HTMLInputElement>) => {
      addFiles(Array.from(event.target.files ?? []));
      // Reset so picking the SAME file again still fires a change event.
      event.target.value = '';
    },
    [addFiles],
  );

  const handleAttachClick = useCallback(() => {
    fileInputRef.current?.click();
  }, []);

  const handleRemoveAttachment = useCallback((id: string) => {
    setAttachments((prev) => prev.filter((entry) => entry.id !== id));
  }, []);

  const handleDragOver = useCallback(
    (event: DragEvent<HTMLDivElement>) => {
      event.preventDefault();
      if (disabled || isSending) return;
      if (!isDragOver) setIsDragOver(true);
    },
    [disabled, isSending, isDragOver],
  );

  const handleDragLeave = useCallback((event: DragEvent<HTMLDivElement>) => {
    const relatedTarget = event.relatedTarget as Node | null;
    if (!event.currentTarget.contains(relatedTarget)) setIsDragOver(false);
  }, []);

  const handleDrop = useCallback(
    (event: DragEvent<HTMLDivElement>) => {
      event.preventDefault();
      setIsDragOver(false);
      if (disabled || isSending) return;
      addFiles(Array.from(event.dataTransfer.files));
    },
    [disabled, isSending, addFiles],
  );

  return (
    <div
      className={`elitea-assistant-input-area${isDragOver ? ' elitea-assistant-input-area--drag-over' : ''}`}
      onDragOver={handleDragOver}
      onDragLeave={handleDragLeave}
      onDrop={handleDrop}
    >
      {isDragOver && (
        <div className="elitea-assistant-drop-overlay">
          {t('widgets.supportAssistant.dropFilesHere', 'Drop files here')}
        </div>
      )}
      {attachments.length > 0 && (
        // Reuses the vendored theme's OWN chip classes (theme/styles/input.css)
        // — pre-existing, unused, since before this change.
        <div className="elitea-assistant-file-list">
          {attachments.map((entry) => (
            <AttachmentChip
              key={entry.id}
              entry={entry}
              disabled={isSending}
              onRemove={() => handleRemoveAttachment(entry.id)}
            />
          ))}
        </div>
      )}
      {validationError && <div className="elitea-assistant-attachment-error">{validationError}</div>}
      <div className="elitea-assistant-input-row">
        <input
          ref={fileInputRef}
          type="file"
          multiple
          className="elitea-assistant-attachment-input"
          onChange={handleFileSelected}
          disabled={disabled}
          hidden
        />
        <button
          type="button"
          className="elitea-assistant-attach-button"
          onClick={handleAttachClick}
          disabled={disabled}
          aria-label={t('widgets.supportAssistant.attach', 'Attach a file')}
        >
          <AttachmentIcon />
        </button>
        <textarea
          id="elitea-assistant-message-input"
          className="elitea-assistant-input"
          value={text}
          onChange={(event) => onTextChange(event.target.value)}
          onKeyDown={handleKeyDown}
          onPaste={handlePaste}
          placeholder={placeholder}
          rows={1}
          disabled={disabled}
        />
        <button
          className="elitea-assistant-send-button"
          onClick={handleSend}
          disabled={isSendDisabled}
          aria-label={t('widgets.supportAssistant.send', 'Send message')}
          type="button"
        >
          <SendIcon />
        </button>
      </div>
    </div>
  );
});

MessageInput.displayName = 'MessageInput';

/** One attachment's chip: name + a status icon (queued/spinner/check/error) + remove. */
function AttachmentChip({
  entry,
  disabled,
  onRemove,
}: {
  readonly entry: TAttachmentEntry;
  readonly disabled: boolean;
  readonly onRemove: () => void;
}) {
  const modifier =
    entry.status === 'done' ? ' elitea-assistant-file-chip--completed' : entry.status === 'error' ? ' elitea-assistant-file-chip--error' : '';
  return (
    <div className={`elitea-assistant-file-chip${modifier}`}>
      <span className="elitea-assistant-file-chip-icon">
        {entry.status === 'uploading' && <SpinnerIcon />}
        {entry.status === 'pending' && <FileIcon />}
        {entry.status === 'done' && <CheckIcon />}
        {entry.status === 'error' && (
          <span className="elitea-assistant-file-chip-error-icon">
            <ErrorIcon />
          </span>
        )}
      </span>
      <span className="elitea-assistant-file-chip-name">{entry.file.name}</span>
      <button
        type="button"
        className="elitea-assistant-file-chip-remove"
        onClick={onRemove}
        disabled={disabled}
        aria-label={t('widgets.supportAssistant.removeAttachmentNamed', 'Remove {{name}}', { name: entry.file.name })}
      >
        <CloseIcon />
      </button>
    </div>
  );
}

export default MessageInput;
