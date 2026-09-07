/**
 * MessageInput — the assistant's message box.
 *
 * # ATTACHMENTS (issue #625 item 2)
 *
 * The published widget's input carries a paperclip, drag-and-drop, clipboard
 * paste, per-file progress chips and a chunked uploader. This port keeps ONE
 * file at a time, picked through a plain `<input type="file">`, with no
 * drag-and-drop, no clipboard paste, and no upload-progress chip — those
 * three are real capability gaps against the reference, kept out
 * deliberately to stay a small, provably-working control rather than a
 * half-ported version of the full one.
 *
 * A PREVIOUS version of this component (and this comment) removed the
 * paperclip entirely, reasoning that "this platform's agent-execution start
 * contract has no attachments field, and the route refuses a non-empty
 * `attachments_info`". That refusal is real (`internal/api/v2/
 * agentexecution/route.go`'s `AttachmentsInfo` gate), but it names a
 * DIFFERENT, still-unported field — `payload.attachments`
 * (`currentStartAttachment`) is a THIRD field on the same body, and #606 gave
 * it a working path all the way to `chat_messages_attachment` rows. Support's
 * own `Predict` route (`internal/api/v2/supportassistant/predict.go`) reads
 * an `attachments` field the identical way. So a picked file DOES reach the
 * agent now: `useChat`'s `handleSend` uploads it
 * (`adapter.api.ts`'s `uploadAttachment`) before starting the turn, and
 * includes the resulting filepath in `startTurn`'s own `attachments` field.
 *
 * The file is cleared the moment `onSend` fires, successfully or not — same
 * as the composer text. A failed send is reported by `useChat`'s own error
 * message, not by this component holding the file for a manual retry.
 */
import type { ChangeEvent, KeyboardEvent } from 'react';
import { memo, useCallback, useMemo, useRef, useState } from 'react';

import { AttachmentIcon, CloseIcon, FileIcon, SendIcon } from '../icons';

import { t } from '@/shared/i18n';

type TMessageInputProps = {
  placeholder: string;
  text: string;
  onTextChange: (text: string) => void;
  onSend: (text: string, file?: File) => void;
  disabled?: boolean | undefined;
};

const MessageInput = memo((props: TMessageInputProps) => {
  const { placeholder, text, onTextChange, onSend, disabled } = props;
  const [attachedFile, setAttachedFile] = useState<File | undefined>(undefined);
  const fileInputRef = useRef<HTMLInputElement>(null);

  // Attaching a file does not waive the message: `content` stays required
  // (SupportPredictPayload.content is min_length=1 server-side), so a file
  // with no words still cannot be sent.
  const isSendDisabled = useMemo(() => Boolean(disabled) || text.trim() === '', [disabled, text]);

  const handleSend = useCallback(() => {
    const trimmed = text.trim();
    if (!trimmed || isSendDisabled) return;
    onTextChange('');
    onSend(trimmed, attachedFile);
    setAttachedFile(undefined);
  }, [text, isSendDisabled, onTextChange, onSend, attachedFile]);

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

  const handleFileSelected = useCallback((event: ChangeEvent<HTMLInputElement>) => {
    const [file] = event.target.files ?? [];
    if (file) setAttachedFile(file);
    // Reset so picking the SAME file again still fires a change event.
    event.target.value = '';
  }, []);

  const handleAttachClick = useCallback(() => {
    fileInputRef.current?.click();
  }, []);

  const handleClearAttachment = useCallback(() => setAttachedFile(undefined), []);

  return (
    <div className="elitea-assistant-input-area">
      {attachedFile && (
        // Reuses the vendored theme's OWN chip classes (theme/styles/input.css)
        // — left in place, unused, when a previous version of this component
        // removed the attach control entirely.
        <div className="elitea-assistant-file-list">
          <div className="elitea-assistant-file-chip">
            <span className="elitea-assistant-file-chip-icon">
              <FileIcon />
            </span>
            <span className="elitea-assistant-file-chip-name">{attachedFile.name}</span>
            <button
              type="button"
              className="elitea-assistant-file-chip-remove"
              onClick={handleClearAttachment}
              aria-label={t('widgets.supportAssistant.removeAttachment', 'Remove attachment')}
            >
              <CloseIcon />
            </button>
          </div>
        </div>
      )}
      <div className="elitea-assistant-input-row">
        <input
          ref={fileInputRef}
          type="file"
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

export default MessageInput;
