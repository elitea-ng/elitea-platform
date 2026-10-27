/**
 * Renders a user's message row. Ported from `apps/elitea-ui/src/[fsd]/
 * features/chat/ui/chat-box/UserMessage.jsx`, and drawn as a chat: the
 * reader's own question is a tinted bubble on the right, hugging its text;
 * anyone else's is a neutral bubble on the left under their name. The
 * baseline's `<avatar> <author> to <recipient>` caption line is gone — the
 * side says who wrote it, and the time rides in the action row.
 */
import type { ChangeEvent, ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import type { Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { MessageAttachmentList } from '../attachments/MessageAttachmentList';
import { replyActionsRevealSx } from './ReplyActionButton';
import { UserMessageActions } from './UserMessageActions';
import { TEXT_MESSAGE_ITEM_TYPE, findAttachmentItems, findQuestionItem, getItemTextContent } from './UserMessage.items';

import { t } from '@/shared/i18n';

import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';

/**
 * The row shell: a column whose children hug the reader's side (right) or
 * the author's (left). The gap between turns is the list's (`ChatMessageList`),
 * not padding here. Hovering it reveals the action row under the bubble.
 */
function userMessageContainerSx(isOwn: boolean) {
  return {
    display: 'flex',
    flexDirection: 'column',
    alignItems: isOwn ? 'flex-end' : 'flex-start',
    alignSelf: 'stretch',
    width: '100%',
    gap: 0.5,
    ...replyActionsRevealSx,
  } as const;
}

/**
 * The bubble the question sits in, hugging its text up to 80% of the column:
 * the selection accent tint for the reader's own, the composer's neutral fill
 * for anyone else's. No shadow, no border.
 */
function userMessageBodySx(isOwn: boolean) {
  return (theme: Theme) => ({
    width: 'fit-content',
    maxWidth: '80%',
    boxSizing: 'border-box',
    background: isOwn
      ? theme.vars.palette.background.select.selected.default
      : theme.vars.palette.background.userInputBackground,
    color: theme.vars.palette.text.secondary,
    borderRadius: theme.vars.shape.radiusLg,
    padding: '0.625rem 1rem',
    position: 'relative',
  } as const);
}

/** baseline `styles.textContent`. */
const messageTextSx = {
  whiteSpace: 'pre-wrap',
  overflowWrap: 'break-word',
  wordBreak: 'break-word',
} as const;

/** A single updated message item sent to `onSubmit` on save (baseline: `updatedItems`). */
export interface UserMessageUpdatedItem {
  readonly uuid?: string | undefined;
  readonly content: string;
  readonly item_type: string;
}

/** @public Props for `UserMessage`. */
export interface UserMessageProps {
  /** The user message to render. */
  readonly message: ChatMessage;
  /** Message ID for tracking. */
  readonly messageId: string;
  /**
   * Whether the signed-in reader wrote this message (the list decides —
   * `ChatMessageList`'s `isOtherAuthor`): a right-hand accent bubble with no
   * name. Otherwise a left-hand neutral bubble under the author's name.
   */
  readonly isOwn?: boolean;
  /** Whether the message is loading/streaming. */
  readonly isLoading?: boolean;
  /** Whether the message is streaming. */
  readonly isStreaming?: boolean;
  /** Whether the message is being regenerated. */
  readonly isRegenerating?: boolean;
  /** Whether the message is the last in the list. */
  readonly isLastMessage?: boolean;
  /** Called when the user clicks the copy button. */
  readonly onCopy?: (() => void) | undefined;
  /** Called when the user clicks the delete button. */
  readonly onDelete?: (() => void) | undefined;
  /** "Fork from here": a new conversation up to and including this message. */
  readonly onFork?: (() => void) | undefined;
  /** Called when the user clicks the regenerate button. */
  readonly onRegenerate?: (() => void) | undefined;
  /** Whether regeneration is currently disabled. */
  readonly shouldDisableRegenerate?: boolean;
  /** Whether to show the auto-speak button. */
  readonly showSpeakButton?: boolean;
  /** Called when the user clicks auto-speak. */
  readonly onAutoSpeak?: (() => void) | undefined;
  /** The current speaking message ID (for sync highlighting). */
  readonly speakingMessageId?: string;
  /** Current speaking segments (for TTS sync). */
  readonly speakingSegments?: readonly unknown[];
  /** Current spoken range (for TTS sync). */
  readonly spokenRange?: { readonly start: number; readonly end: number };
  /**
   * Called with `(messageId, updatedItems)` when the user saves an edit —
   * also gates whether the Edit button/edit flow is offered at all
   * (baseline: `verticalMode && onSubmit`).
   */
  readonly onSubmit?: ((messageId: string, updatedItems: readonly UserMessageUpdatedItem[]) => void) | undefined;
  /** Called when the user confirms removing an attachment from this message. */
  readonly onRemoveAttachment?: ((fileName: string, fromStorage: boolean) => void) | undefined;
  /** Opens a text-like attachment in the canvas editor (issue #878). Omitted, attachment cards offer no such control. */
  readonly onOpenFileInCanvas?: ((source: { readonly bucket: string; readonly name: string }) => void) | undefined;
  /**
   * Required to download an artifact-storage-backed attachment —
   * `NormalAttachment`'s storage branch refuses (via its error report) when
   * this is absent, so leaving it unthreaded made every storage-backed
   * download a silent no-op.
   */
  readonly projectId?: string | undefined;
}

/**
 * Who to caption someone else's question with: whoever the message NAMES,
 * and nobody otherwise. (The reader's own question carries no caption.)
 *
 * `message.name` covers every author the transcript actually states —
 * including the literal "User No Longer Available" that `entities/message`'s
 * normaliser produces for an author id that resolves to nobody, which is a
 * real fact about the message and must not be overwritten.
 *
 * It is empty only when the message names no author AT ALL, which is exactly
 * what the persisted message-list endpoint returns for EVERY row: `GET
 * /elitea_core/messages/prompt_lib/{project}/{conversation}` answers
 * `{id, uid, conversation_id, role, content, ...}` with no author field
 * (see `getMessageAuthorName` in `entities/message/lib/normalise.ts`).
 *
 * There is deliberately no signed-in-user fallback here. Nothing in such a
 * row tells the reader's own question apart from anyone else's, so
 * substituting the reader's name captioned EVERY user bubble of a reloaded
 * SHARED conversation — other people's included — with whoever happened to
 * open it. `userId` could not guard against that either: the endpoint omits
 * that field too, so the guard only ever fired on the live paths, which
 * already carry a name of their own — the send path stamps the signed-in
 * user's name onto the optimistic row (`buildOptimisticUserMessage` reading
 * `buildUserParticipant`, off the same `GET /social/author` this row used to
 * re-read), and the socket echo resolves its author from the conversation's
 * participants (`chatStreamMessageSyncFrames.ts`).
 *
 * An empty caption renders as NO caption (the `Typography` below is gated on
 * it), which is honest; an invented one is not.
 */
function resolveAuthorCaption(message: ChatMessage): string {
  return message.name || '';
}

/**
 * `UserMessage` — renders a user's message content with optional action
 * buttons (copy/edit/delete), an edit-and-resubmit flow, and attachments.
 * Shows the content in a bubble on the reader's or the author's side and —
 * when `onSubmit` is supplied — an Edit button that swaps the bubble for a
 * column-wide textarea with Save/Cancel actions.
 */
export function UserMessage({
  message,
  messageId,
  isOwn = true,
  isLoading = false,
  isStreaming = false,
  onCopy,
  onDelete,
  onFork,
  onSubmit,
  onRemoveAttachment,
  onOpenFileInCanvas,
  projectId,
}: UserMessageProps): ReactNode {
  const authorCaption = isOwn ? '' : resolveAuthorCaption(message);
  const questionItem = useMemo(() => findQuestionItem(message.messageItems), [message.messageItems]);
  const attachmentItems = useMemo(() => findAttachmentItems(message.messageItems), [message.messageItems]);
  const resolvedContent = useMemo(
    () => message.content || getItemTextContent(questionItem) || '',
    [message.content, questionItem],
  );

  const [value, setValue] = useState(resolvedContent);
  const [isEditing, setIsEditing] = useState(false);
  // The attachment cards report download/image failures through a callback
  // that used to be dropped on the floor here — with no toast infrastructure
  // in this app, the message row itself surfaces the last failure as an
  // inline `role="alert"` line (the same pattern
  // `pages/agents/EditApplication.tsx:336` uses for its save errors).
  const [attachmentError, setAttachmentError] = useState<string | null>(null);

  const handleAttachmentError = useCallback((message: string) => {
    setAttachmentError(message);
  }, []);

  const handleEditClick = useCallback(() => {
    setValue(resolvedContent);
    setIsEditing(true);
  }, [resolvedContent]);

  const handleCancel = useCallback(() => {
    setIsEditing(false);
    setValue(resolvedContent);
  }, [resolvedContent]);

  const handleChange = useCallback((event: ChangeEvent<HTMLTextAreaElement | HTMLInputElement>) => {
    setValue(event.target.value);
  }, []);

  const handleSubmit = useCallback(() => {
    // issue 980: ALWAYS one item, whether or not this message carries a stored
    // `text_message` item. It used to send `[]` when `questionItem` was
    // absent, and absent is the ordinary case for a question the browser is
    // still holding from the send that created it — a live-streamed group has
    // no `messageItems` until the transcript is re-read. The consumer
    // (`useChatBoxActions.handleSubmitEditedMessage`) derives the new text
    // from THIS array and returns silently when it is blank, so the editor
    // closed, the bubble updated locally, and not one request left the
    // browser: the edit was lost on reload with no error anywhere.
    //
    // `uuid` is carried only when the message really has one. The field
    // identifies WHICH stored item to rewrite; inventing one would ask the
    // server to update a row that does not exist.
    const uuid = questionItem?.uuid;
    const updatedItems: readonly UserMessageUpdatedItem[] = [
      {
        ...(typeof uuid === 'string' ? { uuid } : {}),
        content: value,
        item_type: TEXT_MESSAGE_ITEM_TYPE,
      },
    ];
    setIsEditing(false);
    onSubmit?.(messageId, updatedItems);
  }, [messageId, onSubmit, questionItem, value]);

  return (
    <Box
      data-testid="user-message"
      sx={userMessageContainerSx(isOwn)}
    >
      {authorCaption !== '' && (
        <Typography
          variant="bodySmall"
          data-testid="chat-message-author"
          sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics, maxWidth: '80%' })}
          noWrap
        >
          {authorCaption}
        </Typography>
      )}
      {isEditing ? (
        <Box sx={{ width: '100%' }}>
          <TextField
            value={value}
            onChange={handleChange}
            multiline
            maxRows={15}
            fullWidth
            size="small"
            variant="standard"
            slotProps={{ input: { disableUnderline: true } }}
            sx={(theme) => ({
              borderRadius: theme.vars.shape.radiusMd,
              border: `1px solid ${theme.vars.palette.border.userMessageEditor}`,
              background: theme.vars.palette.background.userInputBackground,
              px: 1.5,
              py: 1,
            })}
          />
          {attachmentItems.length > 0 && (
            <MessageAttachmentList
              items={attachmentItems}
              {...(onRemoveAttachment !== undefined ? { onRemoveAttachment } : {})}
              {...(projectId !== undefined ? { projectId } : {})}
              onError={handleAttachmentError}
            />
          )}
          <Box sx={{ display: 'flex', flexDirection: 'row-reverse', gap: 1, mt: 1 }}>
            <Button
              size="small"
              variant="contained"
              // issue 980: BLANK ONLY. `value === resolvedContent` used to disable it
              // too, which made "Save and apply" unreachable for an unchanged
              // question — and that control is a RETRY as well as an edit
              // (onetest ELITEA-0540): re-running the same question after a
              // bad answer is the reason a person opens the editor and changes
              // nothing. An empty one stays disabled, because there is no
              // question left to ask.
              disabled={!value.trim()}
              onClick={handleSubmit}
            >
              {t('features.chatMessages.saveAndApply', 'Save and apply')}
            </Button>
            <Button
              size="small"
              variant="outlined"
              onClick={handleCancel}
            >
              {t('features.chatMessages.cancel', 'Cancel')}
            </Button>
          </Box>
        </Box>
      ) : (
        <Box
          data-testid="chat-message-body"
          sx={userMessageBodySx(isOwn)}
        >
          <Typography
            variant="bodyMedium"
            sx={messageTextSx}
          >
            {isLoading || isStreaming
              ? t('features.chatMessages.typing', 'Typing...')
              : resolvedContent}
          </Typography>
          {message.interjected === true && (
            <Typography
              variant="labelSmall"
              color="text.secondary"
              data-testid="chat-message-interjected"
              sx={{ display: 'block', mt: 0.5 }}
            >
              {t('features.chatMessages.sentWhileRunning', 'Sent while running')}
            </Typography>
          )}
          <MessageAttachmentList
            items={attachmentItems}
            {...(onRemoveAttachment !== undefined ? { onRemoveAttachment } : {})}
            {...(projectId !== undefined ? { projectId } : {})}
            {...(onOpenFileInCanvas !== undefined ? { onOpenFileInCanvas } : {})}
            onError={handleAttachmentError}
          />
        </Box>
      )}
      {!isEditing && (
        <UserMessageActions
          onCopy={onCopy}
          onEdit={onSubmit ? handleEditClick : undefined}
          onDelete={onDelete}
          onFork={onFork}
          isOwn={isOwn}
          createdAt={message.createdAt}
        />
      )}
      {attachmentError !== null && (
        <Typography
          variant="bodySmall"
          color="error"
          role="alert"
          data-testid="attachment-error"
          sx={{ mt: 0.5 }}
        >
          {attachmentError}
        </Typography>
      )}
    </Box>
  );
}
