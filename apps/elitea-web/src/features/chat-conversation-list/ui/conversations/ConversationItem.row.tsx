import type { ReactNode } from 'react';

import GroupOutlinedIcon from '@mui/icons-material/GroupOutlined';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import type { ControlsDropdownItem } from '@/shared/ui/ControlsDropdown';
import { ControlsDropdown } from '@/shared/ui/ControlsDropdown';
import { PinIcon } from '@/shared/ui/icons/pin-icon';
import { PlayIcon } from '@/shared/ui/icons/play-icon';

import type { ConversationItemStyles } from './ConversationItem.styles';
import { menuIconStyle } from './ConversationItem.styles';
import type { getConversationType } from './ConversationItem.menu';

/**
 * The non-editing row body — `ConversationItem.jsx:358-454`, split into its
 * own presentational component purely to keep `ConversationItem.tsx` under
 * the §3.5 `max-lines`/`complexity` budgets (moving this render tree's own
 * branching — playback icon / naming-spinner-vs-name / users-icon-by-type /
 * pin indicator / menu — off the parent function's own complexity count,
 * the same "extract a render chunk into its own function" technique this
 * codebase uses throughout, e.g. `shared/ui/modal/BaseModal`'s
 * `ModalHeader`/`ModalActions` split).
 */
export interface ConversationItemRowProps {
  readonly conversationId: string;
  readonly name: string;
  readonly firstMessagePreview: string;
  readonly isPlayback: boolean;
  readonly isPinned: boolean;
  readonly isNamingPending: boolean;
  readonly conversationType: ReturnType<typeof getConversationType>;
  readonly mainBodyWidth: string;
  readonly styles: ConversationItemStyles;
  readonly menuItems: readonly ControlsDropdownItem[];
  readonly onClick: () => void;
  readonly onMouseEnter: () => void;
  readonly onMouseLeave: () => void;
}

export function ConversationItemRow(props: ConversationItemRowProps): ReactNode {
  const { conversationId, name, firstMessagePreview, isPlayback, isPinned, isNamingPending, conversationType, mainBodyWidth, styles, menuItems, onClick, onMouseEnter, onMouseLeave } = props;

  return (
    <Box
      // Id-keyed, not name-keyed: the rail's rows are addressable from an E2E
      // journey only by their title text today, and two conversations may
      // legitimately share one (the name is derived from the question that
      // opened them). `#conversation-menu-{id}-trigger` — the kebab's DOM id —
      // already identifies the row by id, but only from the menu outwards, so
      // "this row left the rail" could not be asserted on the row itself.
      data-testid={`conversation-item-${conversationId}`}
      sx={styles.conversationContentWrapper}
      onClick={onClick}
      onMouseEnter={onMouseEnter}
      onMouseLeave={onMouseLeave}
    >
      {isPlayback && (
        <Box sx={styles.playbackIconWrapper}>
          <PlayIcon style={menuIconStyle} />
        </Box>
      )}
      <Box sx={{ width: mainBodyWidth }}>
        <Box sx={styles.mainBody}>
          {isNamingPending ? (
            <Box
              data-testid="conversation-naming-spinner"
              sx={{ display: 'flex', alignItems: 'center', gap: '.375rem' }}
            >
              <CircularProgress
                size={14}
                thickness={5}
              />
              <Typography
                variant="bodySmall"
                color="text.disabled"
              >
                {t('features.chatConversationList.conversationItem.naming', 'Naming')}
              </Typography>
            </Box>
          ) : (
            <Typography
              sx={styles.nameText}
              component="div"
              variant="bodySmall"
              color="text.secondary"
            >
              {name || firstMessagePreview}
            </Typography>
          )}
        </Box>
      </Box>
      <Box sx={styles.conversationIconWrapper}>
        <ConversationTypeIcon conversationType={conversationType} />
        {isPinned && !isPlayback && <PinIcon style={{ width: '.875rem', height: '.875rem' }} />}
      </Box>
      {!isNamingPending && (
        <Box sx={styles.menuWrapper}>
          <ControlsDropdown
            id={`conversation-menu-${conversationId}`}
            items={[...menuItems]}
          />
        </Box>
      )}
    </Box>
  );
}

function ConversationTypeIcon({ conversationType }: { readonly conversationType: ReturnType<typeof getConversationType> }): ReactNode {
  if (conversationType === 'private_with_users') {
    return <GroupOutlinedIcon fontSize="small" sx={groupIconDefaultSx} />;
  }
  if (conversationType === 'public') {
    return <GroupOutlinedIcon fontSize="small" sx={groupIconPublishedSx} />;
  }
  return null;
}

const groupIconDefaultSx = (theme: Theme) => ({ color: theme.vars.palette.icon.fill.default });
const groupIconPublishedSx = (theme: Theme) => ({ color: theme.vars.palette.status.published });
