/**
 * The wiki chat's past sessions — list, resume, delete (#873).
 *
 * WHAT THIS ADDS. Server-side history already let a browser CONTINUE its own
 * conversation across a reload; what it could not do is show the reader that
 * OTHER conversations exist for this toolkit, let them switch to one, or
 * remove one. This is that surface: a list of the toolkit's stored wiki
 * conversations (from `listWikiConversations`, already fetched by the
 * drawer's history query — this component renders the array, it does not
 * fetch a second one), the current session marked, and a delete action per
 * row.
 *
 * RESUME IS "ADOPT THE KEY". Which conversation a question is filed under is
 * decided entirely by `X-Elitea-Wiki-Chat` — the opaque key
 * `WikiConversationKey` holds — so resuming a past session means adopting
 * ITS key (`conversation.chatKey`) and re-hydrating the transcript, exactly
 * the mechanism the drawer already uses to adopt a conversation on a second
 * device. A session with no key cannot be resumed — every deepwiki-recorded
 * conversation has one, so an entry without it is a data anomaly rather than
 * the ordinary case, and Resume is disabled on it rather than sent to a key
 * that would file the next question in the wrong place.
 *
 * DELETE GOES THROUGH THE ORDINARY CONVERSATION ROUTE. No new authorization
 * rule: `deleteWikiConversation` is the exact same `DELETE
 * /conversation/prompt_lib/{projectId}/{id}` the chat page's own
 * conversation list calls. Confirmed with `DeleteEntityModal`, the app's
 * standing delete-confirmation surface — a wiki transcript is exactly the
 * kind of thing "click and it's gone" should not apply to.
 */
import { memo, useCallback, useState } from 'react';

import Box from '@mui/material/Box';
import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import HistoryIcon from '@mui/icons-material/History';
import IconButton from '@mui/material/IconButton';
import ListItemText from '@mui/material/ListItemText';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

import type { WikiConversationSummary } from '../api/wikiHistoryApi';

export interface WikiChatSessionsProps {
  /** This toolkit's stored wiki conversations, newest first. */
  readonly conversations: readonly WikiConversationSummary[];
  /** The conversation id the drawer currently has open, if any is loaded. */
  readonly currentConversationId: string | undefined;
  readonly onResume: (conversation: WikiConversationSummary) => void;
  readonly onDelete: (conversation: WikiConversationSummary) => void;
  readonly disabled: boolean;
  /**
   * Fired the moment the menu opens, BEFORE `conversations` is read — the
   * drawer's own listing is fetched once per `historyEpoch`
   * (`WikiChatDrawer.tsx`) and nothing bumps it after an ordinary send, only
   * after Clear/Resume/Delete. Without this, asking a first question in a
   * session started by Clear left that session itself missing from ITS OWN
   * list until some unrelated epoch bump happened to run — a reader who
   * asked one question and immediately opened "Past conversations" would
   * not find the conversation they were just having.
   */
  readonly onOpen: () => void;
}

/** A row's label: the stored name, falling back to the id for one with none. */
function sessionLabel(conversation: WikiConversationSummary): string {
  return conversation.name !== '' ? conversation.name : conversation.id;
}

export const WikiChatSessions = memo(function WikiChatSessions({
  conversations,
  currentConversationId,
  onResume,
  onDelete,
  disabled,
  onOpen,
}: WikiChatSessionsProps) {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const [pendingDelete, setPendingDelete] = useState<WikiConversationSummary | null>(null);

  const openMenu = useCallback(
    (event: { currentTarget: HTMLElement }) => {
      setAnchor(event.currentTarget);
      onOpen();
    },
    [onOpen],
  );
  const closeMenu = useCallback(() => {
    setAnchor(null);
  }, []);

  const resume = useCallback(
    (conversation: WikiConversationSummary) => {
      closeMenu();
      onResume(conversation);
    },
    [closeMenu, onResume],
  );

  const requestDelete = useCallback((conversation: WikiConversationSummary) => {
    setPendingDelete(conversation);
  }, []);

  const confirmDelete = useCallback(() => {
    if (pendingDelete === null) return;
    // Closed BEFORE the caller's delete resolves: a session list re-fetch
    // that lands while this dialog is still open would otherwise be reading
    // `pendingDelete` against an array that no longer contains it.
    const conversation = pendingDelete;
    setPendingDelete(null);
    closeMenu();
    onDelete(conversation);
  }, [closeMenu, onDelete, pendingDelete]);

  const label = t('widgets.deepwiki.chat.sessions', 'Past conversations');

  if (conversations.length === 0) return null;

  return (
    <>
      <Tooltip title={label}>
        <span>
          <IconButton
            size="small"
            onClick={openMenu}
            disabled={disabled}
            aria-label={label}
            data-testid="wiki-chat-sessions-button"
          >
            <HistoryIcon fontSize="small" />
          </IconButton>
        </span>
      </Tooltip>

      <Menu open={anchor !== null} anchorEl={anchor} onClose={closeMenu}>
        {conversations.map((conversation) => (
          <MenuItem
            key={conversation.id}
            selected={conversation.id === currentConversationId}
            onClick={() => {
              resume(conversation);
            }}
            disabled={conversation.chatKey === undefined}
            data-testid="wiki-chat-session-option"
          >
            <ListItemText
              primary={sessionLabel(conversation)}
              secondary={conversation.updatedAt}
            />
            <Box
              component="span"
              onClick={(event) => {
                // A delete inside a resume row must not also resume it.
                event.stopPropagation();
                requestDelete(conversation);
              }}
            >
              <IconButton
                size="small"
                aria-label={t('widgets.deepwiki.chat.deleteSession', 'Delete this conversation')}
                data-testid="wiki-chat-session-delete"
              >
                <DeleteOutlinedIcon fontSize="small" />
              </IconButton>
            </Box>
          </MenuItem>
        ))}
      </Menu>

      <DeleteEntityModal
        open={pendingDelete !== null}
        onClose={() => {
          setPendingDelete(null);
        }}
        onConfirm={confirmDelete}
        // exactOptionalPropertyTypes: `name` is only added when there is a
        // pending session, rather than passed as an explicit `undefined`.
        {...(pendingDelete ? { name: sessionLabel(pendingDelete) } : {})}
        copy={{
          title: t('widgets.deepwiki.chat.deleteSessionTitle', 'Delete conversation'),
        }}
        data-testid="wiki-chat-session-delete-modal"
      />
    </>
  );
});
