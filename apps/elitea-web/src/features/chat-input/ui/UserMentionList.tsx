/**
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/chat/ui/user-mention-list/
 * UserMentionList.jsx` — the "@" user-mention dropdown. Has zero
 * cross-feature/Redux coupling in the baseline (confirmed by reading the
 * whole file): it self-manages filtering + keyboard nav entirely from its
 * own props, so this is a straightforward, standalone port — no
 * accompanying hook (unlike the "/" and "~" systems). Neither
 * `UserInput.jsx` nor `NewChatInput.jsx` (the old app's own message-input
 * components) render any suggestion popup — composed one layer up
 * (`ChatBox.jsx`) — so, matching every other component in this unit, this
 * is a standalone, exportable piece: the future composition-root unit (C6)
 * derives the `users`/`query` props from wherever it tracks the "@" trigger
 * position and the active conversation's participants, and renders this on
 * top.
 *
 * **`users` prop shape simplification (disclosed).** The baseline's `users`
 * items look like `{id, name, participant: {meta: {user_name, user_avatar}}}`
 * — a synthetic wrapper `ChatBox.jsx` builds around each raw conversation
 * participant (plus a synthetic `{id: '@everyone', name: 'Everyone',
 * participant: 'All users'}` entry). `UserMentionCandidate` below flattens
 * that one level (`avatarName`/`avatarUrl` fields directly, no nested
 * `.participant.meta` chain) — the caller (which already has the full
 * `entities/participant` `Participant` row, with clean camelCase
 * `meta.userName`/`meta.userAvatar`) resolves these two fields however it
 * likes, including for a synthetic "Everyone" entry (`avatarName`/
 * `avatarUrl` both omitted is a legal `UserMentionCandidate`, same as the
 * baseline's own "Everyone" entry has no participant data to read an
 * avatar from).
 *
 * **Keyboard nav — preserved as-is, not migrated to a shared pipeline.**
 * The baseline self-manages `ArrowUp`/`ArrowDown`/`Enter` via a raw
 * `document.addEventListener('keydown', handler, true)` (capture phase),
 * rather than going through `useSlashCommandHandler`'s
 * `onKeyDown`/`itemCountRef`/`onConfirmActiveRef` pipeline every other
 * mention type in this unit uses. Still correct in this architecture: this
 * component owns its own DOM-level listener independent of whatever
 * textarea `onKeyDown` prop C6 wires up, so it works regardless of which
 * element has focus while the popup is open (matching the baseline's own
 * apparent intent — a document-capture listener beats focus assumptions).
 * Kept verbatim rather than retrofitted onto the ref-based pipeline, since
 * unlike "/"/"~" there is no committed-mention text-replacement state
 * machine here to synchronise with.
 */
import type { MouseEvent, ReactNode } from 'react';
import { memo, useCallback, useEffect, useMemo, useState } from 'react';

import Avatar from '@mui/material/Avatar';
import Box from '@mui/material/Box';
import ClickAwayListener from '@mui/material/ClickAwayListener';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { getInitials, stringToColor } from '@/shared/lib/string';
import { t } from '@/shared/i18n';

/** One user selectable from the "@" dropdown — see the module doc comment for why this flattens the baseline's `{id, name, participant: {meta: {...}}}` shape. */
export interface UserMentionCandidate {
  readonly id: string;
  readonly name: string;
  /** Falls back to `name` when absent, matching the baseline's `user.participant?.meta?.user_name \|\| user.name`. */
  readonly avatarName?: string | undefined;
  readonly avatarUrl?: string | undefined;
}

interface UserMentionItemProps {
  readonly user: UserMentionCandidate;
  readonly onClick: (user: UserMentionCandidate) => void;
  readonly isActive: boolean;
}

const UserMentionItem = memo(function UserMentionItem({ user, onClick, isActive }: UserMentionItemProps): ReactNode {
  const handleClick = useCallback(
    (event: MouseEvent) => {
      event.stopPropagation();
      event.preventDefault();
      onClick(user);
    },
    [onClick, user],
  );

  const avatarName = user.avatarName ?? user.name;

  return (
    <Box
      onClick={handleClick}
      sx={(theme: Theme) => ({
        display: 'flex',
        flexDirection: 'row',
        alignItems: 'center',
        gap: '0.5rem',
        padding: '0.375rem 0.5rem',
        borderRadius: theme.vars.shape.radiusMd,
        cursor: 'pointer',
        backgroundColor: isActive ? theme.vars.palette.action.hover : 'transparent',
        '&:hover': { backgroundColor: theme.vars.palette.action.hover },
      })}
    >
      {user.avatarUrl ? (
        <Avatar
          src={user.avatarUrl}
          alt={avatarName}
          sx={{ width: '1.5rem', height: '1.5rem' }}
        />
      ) : (
        <Avatar
          alt={avatarName}
          sx={(theme: Theme) => ({
            width: '1.5rem',
            height: '1.5rem',
            backgroundColor: stringToColor(avatarName),
            color: theme.vars.palette.text.secondary,
          })}
        >
          {avatarName ? (
            <Typography
              variant="labelSmall"
              component="span"
              color="inherit"
            >
              {getInitials(avatarName)}
            </Typography>
          ) : null}
        </Avatar>
      )}
      <Typography
        variant="headingSmall"
        color="text.secondary"
        sx={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
      >
        {user.name}
      </Typography>
    </Box>
  );
});

export interface UserMentionListProps {
  readonly users?: readonly UserMentionCandidate[];
  /** The full typed fragment including the leading "@" (e.g. `"@ali"`) — only the part after "@" is used to filter. */
  readonly query?: string | undefined;
  readonly onSelectUser: (user: UserMentionCandidate) => void;
  readonly onClose: () => void;
  /** The list's heading; "Participants" when absent. */
  readonly title?: string | undefined;
  readonly testId?: string | undefined;
}

export function UserMentionList({ users = [], query, onSelectUser, onClose, title, testId }: UserMentionListProps): ReactNode {
  const [activeIndex, setActiveIndex] = useState(0);

  const filteredUsers = useMemo(() => {
    if (!users.length) return [];
    const searchStr = query?.slice(1).toLowerCase() ?? '';
    if (!searchStr) return users;
    return users.filter((u) => u.name.toLowerCase().includes(searchStr));
  }, [users, query]);

  useEffect(() => {
    setActiveIndex(0);
  }, [filteredUsers]);

  useEffect(() => {
    if (!filteredUsers.length) return;

    const handleKeyDown = (event: KeyboardEvent): void => {
      if (event.key === 'ArrowDown') {
        event.preventDefault();
        setActiveIndex((prev) => (prev + 1) % filteredUsers.length);
      } else if (event.key === 'ArrowUp') {
        event.preventDefault();
        setActiveIndex((prev) => (prev - 1 + filteredUsers.length) % filteredUsers.length);
      } else if (event.key === 'Enter') {
        event.preventDefault();
        event.stopPropagation();
        const active = filteredUsers[activeIndex];
        if (active) onSelectUser(active);
      }
    };

    document.addEventListener('keydown', handleKeyDown, true);
    return () => document.removeEventListener('keydown', handleKeyDown, true);
  }, [filteredUsers, activeIndex, onSelectUser]);

  if (!filteredUsers.length) return null;

  return (
    <ClickAwayListener onClickAway={onClose}>
      <Box
        data-testid={testId}
        sx={(theme: Theme) => ({
          border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
          width: '100%',
          maxWidth: '100%',
          maxHeight: '15.4375rem',
          borderRadius: theme.vars.shape.radiusLg,
          boxSizing: 'border-box',
          padding: '0.75rem',
          display: 'flex',
          flexDirection: 'column',
          gap: '0.25rem',
          background: theme.vars.palette.background.secondary,
          overflowY: 'auto',
        })}
      >
        <Box sx={{ height: '1rem', display: 'flex', alignItems: 'center', padding: '0 0.5rem' }}>
          <Typography
            variant="subtitle"
            color="text.primary"
          >
            {title ?? t('chatInput.userMentionList.participants', 'Participants')}
          </Typography>
        </Box>
        <Box sx={{ display: 'flex', flexDirection: 'column', width: '100%' }}>
          {filteredUsers.map((user, index) => (
            <UserMentionItem
              key={user.id}
              user={user}
              onClick={onSelectUser}
              isActive={index === activeIndex}
            />
          ))}
        </Box>
      </Box>
    </ClickAwayListener>
  );
}
