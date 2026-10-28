/**
 * The action row under an assistant reply, drawn as ChatGPT and Claude draw
 * theirs: one compact, left-aligned row under the reply's text, quiet 16px
 * outlined icons (`ReplyActionButton`), the frequent actions inline and the
 * rest behind ⋯.
 *
 *   Copy · Good · Bad · Read out · Regenerate · ⋯ (Remember this, Open as
 *   document, Fork from here, — , Delete) · Using N memories · 2 minutes ago
 *
 * It used to sit inside the card, spread across its full width (the thumbs
 * and bookmark at the left, six icons at the right), at MUI's default
 * icon-button weight. Every control is still here; each renders only when its
 * handler is supplied, as before, and Delete still goes through the
 * caller's confirmation (`onDelete` opens it).
 *
 * Visibility: always under the latest reply (`alwaysVisible`); under an
 * older one it fades in on hover or when focus enters it. It is faded by
 * opacity, not hidden, so Tab still reaches every control, and it stays
 * shown while its ⋯ menu is open (the menu's portal takes the hover away)
 * and on a device with no hover at all.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import ArticleOutlinedIcon from '@mui/icons-material/ArticleOutlined';
import AutorenewIcon from '@mui/icons-material/Autorenew';
import CallSplitOutlinedIcon from '@mui/icons-material/CallSplitOutlined';
import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import MoreHorizIcon from '@mui/icons-material/MoreHoriz';
import VolumeUpOutlinedIcon from '@mui/icons-material/VolumeUpOutlined';
import Menu from '@mui/material/Menu';

import { t } from '@/shared/i18n';
import { MenuDivider, MenuRow, MenuRowIcon, MenuRowText } from '@/shared/ui/native-menu';

import { MessageFeedbackControl } from './MessageFeedbackControl';
import { MessageMeta, type MessageMetaProps } from './MessageMeta';
import { ReplyActionButton, ReplyActionsRow } from './ReplyActionButton';
import { RememberMemoryMenuRow, useRememberMemory, type RememberMemory } from './RememberMemoryAction';

interface ApplicationAnswerActionsState {
  readonly hasContent: boolean;
  readonly isProcessing: boolean;
  readonly shouldDisableRegenerate: boolean;
  readonly hasSpeakableText: boolean;
  readonly isSpeaking: boolean;
}

interface ApplicationAnswerActionsHandlers {
  readonly onAutoSpeak?: (() => void) | undefined;
  readonly onCopy?: (() => void) | undefined;
  readonly onRegenerate?: (() => void) | undefined;
  readonly onDelete?: (() => void) | undefined;
  /** "Fork from here" (client contract 1.11); omitted, no row renders. */
  readonly onFork?: (() => void) | undefined;
  /**
   * Carves the WHOLE answer out into a `document` canvas (issue #879) — the
   * same underlying create route the selection-drag affordance calls, with
   * `kind: 'document'` forced rather than guessed. Omitted — the answer has
   * already been split into items, or the composition root wired no canvas
   * creation at all — no such row renders.
   */
  readonly onOpenAsDocument?: (() => void) | undefined;
}

/** @public Props for `ApplicationAnswerActions`. */
export interface ApplicationAnswerActionsProps {
  readonly state: ApplicationAnswerActionsState;
  readonly handlers: ApplicationAnswerActionsHandlers;
  /** Thumbs and "Remember this" (#880/#870): a resolved project and a settled answer, else `undefined`. */
  readonly feedback?: { readonly projectId: string; readonly messageId: string } | undefined;
  /** The text "Remember this" saves. */
  readonly memoryContent: string;
  /** The latest reply's row never fades. */
  readonly alwaysVisible: boolean;
  /** The muted tail of the row: the "Using N memories" chip and the time. */
  readonly meta?: MessageMetaProps | undefined;
}

function InlineActions({ state, handlers, feedback }: Pick<ApplicationAnswerActionsProps, 'state' | 'handlers' | 'feedback'>): ReactNode {
  const { onCopy, onAutoSpeak, onRegenerate } = handlers;
  return (
    <>
      {onCopy && state.hasContent && (
        <ReplyActionButton label={t('features.chatMessages.replyActions.copy', 'Copy to clipboard')} disabled={state.isProcessing} onClick={onCopy}>
          <ContentCopyIcon />
        </ReplyActionButton>
      )}
      {feedback && <MessageFeedbackControl projectId={feedback.projectId} messageId={feedback.messageId} />}
      {onAutoSpeak && state.hasSpeakableText && (
        <ReplyActionButton label={t('features.chatMessages.replyActions.readOut', 'Read out')} disabled={state.isProcessing || state.isSpeaking} onClick={onAutoSpeak}>
          <VolumeUpOutlinedIcon />
        </ReplyActionButton>
      )}
      {onRegenerate && (
        <ReplyActionButton label={t('features.chatMessages.replyActions.regenerate', 'Regenerate')} disabled={state.shouldDisableRegenerate} onClick={onRegenerate}>
          <AutorenewIcon />
        </ReplyActionButton>
      )}
    </>
  );
}

interface MoreMenuProps {
  readonly state: ApplicationAnswerActionsState;
  readonly handlers: ApplicationAnswerActionsHandlers;
  readonly memory: RememberMemory | undefined;
  readonly anchor: HTMLElement | null;
  readonly onAnchor: (anchor: HTMLElement | null) => void;
}

function hasMoreRows({ state, handlers, memory }: Pick<MoreMenuProps, 'state' | 'handlers' | 'memory'>): boolean {
  return Boolean(memory?.available) || Boolean(handlers.onOpenAsDocument && state.hasContent) || Boolean(handlers.onFork) || Boolean(handlers.onDelete);
}

/** One menu row that runs `action` and closes the menu. */
function ActionRow({ icon, label, testId, disabled, action, close }: {
  readonly icon: ReactNode; readonly label: string; readonly testId: string; readonly disabled?: boolean; readonly action: () => void; readonly close: () => void;
}): ReactNode {
  return (
    <MenuRow data-testid={testId} disabled={disabled} onClick={() => { close(); action(); }}>
      <MenuRowIcon>{icon}</MenuRowIcon>
      <MenuRowText>{label}</MenuRowText>
    </MenuRow>
  );
}

function MoreMenu({ state, handlers, memory, anchor, onAnchor }: MoreMenuProps): ReactNode {
  const { onOpenAsDocument, onFork, onDelete } = handlers;
  if (!hasMoreRows({ state, handlers, memory })) return null;
  const close = (): void => onAnchor(null);
  const label = t('features.chatMessages.replyActions.more', 'More actions');
  const openAsDocument = t('features.chatMessages.canvas.document.openAsDocument', 'Open as document');
  return (
    <>
      <ReplyActionButton label={label} data-testid="answer-more-actions" aria-haspopup="menu" aria-expanded={anchor !== null} onClick={(event) => onAnchor(event.currentTarget)}>
        <MoreHorizIcon />
      </ReplyActionButton>
      <Menu anchorEl={anchor} open={anchor !== null} onClose={close} slotProps={{ list: { dense: true, 'aria-label': label }, paper: { sx: { minWidth: '12rem' } } }}>
        {memory && <RememberMemoryMenuRow memory={memory} disabled={state.isProcessing} onDone={close} />}
        {onOpenAsDocument && state.hasContent && (
          <ActionRow icon={<ArticleOutlinedIcon />} label={openAsDocument} testId="answer-open-as-document" disabled={state.isProcessing} action={onOpenAsDocument} close={close} />
        )}
        {onFork && (
          <ActionRow icon={<CallSplitOutlinedIcon />} label={t('features.chatMessages.forkFromHere', 'Fork from here')} testId="message-fork" disabled={state.isProcessing} action={onFork} close={close} />
        )}
        {onDelete && <MenuDivider />}
        {onDelete && <ActionRow icon={<DeleteOutlinedIcon />} label={t('features.chatMessages.replyActions.delete', 'Delete')} testId="answer-delete" action={onDelete} close={close} />}
      </Menu>
    </>
  );
}

function ActionsRow({ state, handlers, feedback, alwaysVisible, memory, meta }: Omit<ApplicationAnswerActionsProps, 'memoryContent'> & { readonly memory: RememberMemory | undefined }): ReactNode {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const shown = alwaysVisible || anchor !== null;
  return (
    <ReplyActionsRow shown={shown}>
      <InlineActions state={state} handlers={handlers} feedback={feedback} />
      <MoreMenu state={state} handlers={handlers} memory={memory} anchor={anchor} onAnchor={setAnchor} />
      {memory?.toast}
      {meta && <MessageMeta {...meta} />}
    </ReplyActionsRow>
  );
}

/** "Remember this" holds a mutation, so it mounts only with a resolved project (the feedback gate), as it always did. */
function ActionsRowWithMemory(props: ApplicationAnswerActionsProps & { readonly feedback: NonNullable<ApplicationAnswerActionsProps['feedback']> }): ReactNode {
  const memory = useRememberMemory({ projectId: props.feedback.projectId, content: props.memoryContent });
  return <ActionsRow {...props} memory={memory} />;
}

export function ApplicationAnswerActions(props: ApplicationAnswerActionsProps): ReactNode {
  const { feedback } = props;
  return feedback ? <ActionsRowWithMemory {...props} feedback={feedback} /> : <ActionsRow {...props} memory={undefined} />;
}
