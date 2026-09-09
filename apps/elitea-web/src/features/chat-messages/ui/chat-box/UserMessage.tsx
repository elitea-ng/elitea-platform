/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/ui/chat-box/
 * UserMessage.jsx` — renders a user's message row.
 *
 * Port of `apps/elitea-ui/src/[fsd]/features/chat/ui/chat-box/
 * UserMessage.jsx`.
 */
import type { ChangeEvent, ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { MessageAttachmentList } from '../attachments/MessageAttachmentList';
import { MessageAvatar } from './MessageAvatar';
import { MessageHeaderRow } from './MessageHeaderRow';
import { UserMessageActions } from './UserMessageActions';

import type { Attachment } from '@/entities/attachment/model/types';
import { t } from '@/shared/i18n';

import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';
import { useParticipantName } from '../../lib/participantName';
import type { ParticipantNameInput } from '../../lib/participantName';

const TEXT_MESSAGE_ITEM_TYPE = 'text_message';
const ATTACHMENT_ITEM_TYPE = 'attachment_message';

/**
 * The row shell, measured off the production transcript's own `<li>`
 * (`padding: 12px 0; gap: 8px; flex-direction: column; border-radius: 4px`) —
 * baseline `UserMessage.jsx`'s `styles.containerVertical` over
 * `UserMessageContainerWithMargin`. It used to be a right-aligned
 * `row-reverse` bubble, which is not a layout this product has anywhere.
 */
const userMessageContainerSx = {
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'flex-start',
  alignSelf: 'stretch',
  width: '100%',
  gap: '0.5rem',
  padding: '0.75rem 0',
  borderRadius: '0.25rem',
} as const;

/** baseline `styles.messageVertical`: the full-width card the question sits in. */
function userMessageBodySx(theme: import('@mui/material/styles').Theme) {
  return {
    width: '100%',
    boxSizing: 'border-box',
    background: theme.vars.palette.background.aiAnswerBkg,
    color: theme.vars.palette.text.secondary,
    boxShadow: theme.vars.palette.boxShadow.aiAnswer,
    borderRadius: theme.vars.shape.radiusMd,
    padding: '0.75rem 1rem',
    position: 'relative',
  } as const;
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
  /** Called (in addition to entering local edit mode) when the user clicks the edit button. */
  readonly onEdit?: (() => void) | undefined;
  /** Called when the user clicks the delete button. */
  readonly onDelete?: (() => void) | undefined;
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
 * `messageItems`' declared type (`MessageItemWire[]`) only models `id`/
 * `item_details.content` — the real wire also carries `item_type`/`uuid`,
 * read here the same defensive way `convertMessagesToChatHistory.ts` does.
 */
function getRawMessageItems(messageItems: ChatMessage['messageItems']): ReadonlyArray<Record<string, unknown>> {
  return (messageItems ?? []) as unknown as ReadonlyArray<Record<string, unknown>>;
}

/** Finds the `text_message` item within a message's raw `messageItems` (baseline: `questionItem`). */
function findQuestionItem(messageItems: ChatMessage['messageItems']): Record<string, unknown> | undefined {
  return getRawMessageItems(messageItems).find((item) => item.item_type === TEXT_MESSAGE_ITEM_TYPE);
}

/** Filters a message's raw `messageItems` down to attachment-typed items (baseline: `attachmentItems`). */
function findAttachmentItems(messageItems: ChatMessage['messageItems']): readonly Attachment[] {
  const items = getRawMessageItems(messageItems).filter((item) => item.item_type === ATTACHMENT_ITEM_TYPE);
  return items as unknown as readonly Attachment[];
}

/** Reads the text content nested in a `text_message` item's `item_details.content`. */
function getItemTextContent(item: Record<string, unknown> | undefined): string | undefined {
  const details = item?.item_details as { content?: string } | undefined;
  return details?.content;
}

/**
 * Who to caption a question with: whoever the message NAMES, and nobody
 * otherwise.
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
 * An empty caption renders as NO caption line (the `Typography` below is
 * gated on it), which is honest; an invented one is not.
 */
function resolveAuthorCaption(message: ChatMessage): string {
  return message.name || '';
}

/**
 * `UserMessage` — renders a user's message content with optional action
 * buttons (copy/edit/delete), an edit-and-resubmit flow, and attachments.
 * Shows the user's name (from the message), the message content as text,
 * and — when `onSubmit` is supplied — an Edit button that swaps the bubble
 * for a textarea with Save/Cancel actions.
 */
export function UserMessage({
  message,
  messageId,
  isLoading = false,
  isStreaming = false,
  onCopy,
  onEdit,
  onDelete,
  onSubmit,
  onRemoveAttachment,
  onOpenFileInCanvas,
  projectId,
}: UserMessageProps): ReactNode {
  const authorCaption = resolveAuthorCaption(message);
  // Who the question was addressed to — the "to <name>" half of the caption
  // line. `sentTo` is already on every normalised user message
  // (`entities/message/lib/normalise.ts`'s `resolveSentTo`); nothing read it
  // until now, and `lib/participantName.ts` — the ported resolver for exactly
  // this — had no caller at all.
  const sentToParticipant = message.sentTo as ParticipantNameInput | undefined;
  const resolvedSentToName = useParticipantName(sentToParticipant);
  const sentToName = sentToParticipant?.entity_name ? resolvedSentToName : undefined;
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
    onEdit?.();
  }, [resolvedContent, onEdit]);

  const handleCancel = useCallback(() => {
    setIsEditing(false);
    setValue(resolvedContent);
  }, [resolvedContent]);

  const handleChange = useCallback((event: ChangeEvent<HTMLTextAreaElement | HTMLInputElement>) => {
    setValue(event.target.value);
  }, []);

  const handleSubmit = useCallback(() => {
    const updatedItems: readonly UserMessageUpdatedItem[] = questionItem
      ? [{ uuid: questionItem.uuid as string | undefined, content: value, item_type: TEXT_MESSAGE_ITEM_TYPE }]
      : [];
    setIsEditing(false);
    onSubmit?.(messageId, updatedItems);
  }, [messageId, onSubmit, questionItem, value]);

  return (
    <Box
      data-testid="user-message"
      sx={userMessageContainerSx}
    >
      <MessageHeaderRow
        avatar={<MessageAvatar name={authorCaption} avatarUrl={message.avatar} />}
        name={authorCaption}
        sentToName={sentToName}
        createdAt={message.createdAt}
      />
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
              disabled={value === resolvedContent || !value.trim()}
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
          sx={userMessageBodySx}
        >
          <Typography
            variant="bodyMedium"
            sx={messageTextSx}
          >
            {isLoading || isStreaming
              ? t('features.chatMessages.typing', 'Typing...')
              : resolvedContent}
          </Typography>
          <MessageAttachmentList
            items={attachmentItems}
            {...(onRemoveAttachment !== undefined ? { onRemoveAttachment } : {})}
            {...(projectId !== undefined ? { projectId } : {})}
            {...(onOpenFileInCanvas !== undefined ? { onOpenFileInCanvas } : {})}
            onError={handleAttachmentError}
          />
          <UserMessageActions
            onCopy={onCopy}
            onEdit={onSubmit ? handleEditClick : undefined}
            onDelete={onDelete}
          />
        </Box>
      )}
      {attachmentError !== null && (
        <Typography
          variant="caption"
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
