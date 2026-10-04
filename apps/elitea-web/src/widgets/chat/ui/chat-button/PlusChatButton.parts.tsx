/**
 * JSX subcomponents for `PlusChatButton.tsx` — split out purely to keep
 * that component under the §3.5 cyclomatic-complexity-12 budget (each of
 * these is its own function scope, so its internal branches/`.map` don't
 * count toward the composition root's complexity) and file-length-400
 * budget. See `PlusChatButton.helpers.ts` for the pure (non-JSX) helpers.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Paper from '@mui/material/Paper';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { ArrowRightIcon } from '@/shared/ui/icons/arrow-right-icon';

import { AttachmentButton } from './AttachmentButton';
import type { MenuItemDef, SubmenuKey } from './PlusChatButton.helpers';

export interface AttachmentsPanelProps {
  readonly disableAttachments: boolean;
  readonly attachments: readonly File[];
  readonly onAttachFiles: (files: readonly File[]) => void;
  readonly limits: Record<string, number>;
}

export function AttachmentsPanel({
  disableAttachments,
  attachments,
  onAttachFiles,
  limits,
}: AttachmentsPanelProps): ReactNode {
  return (
    <Box sx={{ padding: '0.5rem' }}>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, padding: '0.375rem 0.5rem' }}>
        <AttachmentButton
          disableAttachments={disableAttachments}
          attachments={attachments}
          onAttachFiles={onAttachFiles}
          limits={limits}
        />
        <Typography variant="bodyMedium">{t('widgets.chat.plusChatButton.attachFilesLabel', 'Attach files')}</Typography>
      </Box>
      {attachments.length > 0 && (
        <Box sx={{ maxHeight: '12rem', overflowY: 'auto' }}>
          {attachments.map((file, i) => (
            <Typography
              key={`${file.name}-${i}`}
              variant="bodySmall"
              component="div"
              sx={{ padding: '0.375rem 0.5rem', color: 'text.secondary' }}
            >
              {file.name}
            </Typography>
          ))}
        </Box>
      )}
    </Box>
  );
}

export interface MainMenuListProps {
  readonly items: readonly MenuItemDef[];
  /** Receives the hovered/clicked row element so the submenu can anchor beside it. */
  readonly onSelectSubmenu: (key: SubmenuKey, anchor: HTMLElement) => void;
  /** The "Attach files" row rendered above the categories — see this component's doc. */
  readonly attachRow: ReactNode;
}

const rowSx = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: 1,
  padding: '0.375rem 1rem',
  height: '2.75rem',
  cursor: 'pointer',
  color: theme.vars.palette.text.secondary,
  '&:hover': { backgroundColor: theme.vars.palette.action.hover },
});

const menuIconSx = { width: '1.25rem', height: '1.25rem', flexShrink: 0 } as const;

/**
 * The "+" menu's top level: the attach-files row, then the expandable
 * categories, in the baseline's order (`PlusChatButton.jsx:361-397`).
 *
 * Two things this used to do that the reference does not:
 *
 *  - a **"Add to chat" header row wired to `onBack`** sat above everything.
 *    At the top level there is nothing to go back to, so it was a clickable
 *    row that did nothing; the reference has no such header.
 *  - the chevron was the **literal character `›`** in a `Typography`. It
 *    inherits the font's own metrics, so it sat at a different size and
 *    baseline from every other chevron in the app. It is now `ArrowRightIcon`,
 *    the ported asset the baseline uses here.
 *
 * Rows open their submenu on HOVER as well as click (baseline
 * `onMouseEnter={e => handleItemHover(key, e)}`), and hand back the row
 * element so the caller can anchor the submenu BESIDE it rather than
 * replacing this list with it. There is deliberately no `onMouseLeave`: the
 * submenu is dismissed by opening a different one or by clicking away, so the
 * pointer can travel from the row into the submenu without it vanishing
 * mid-move. That is the failure a leave-handler plus a re-entry timer is
 * usually written to paper over.
 */
export function MainMenuList({ items, onSelectSubmenu, attachRow }: MainMenuListProps): ReactNode {
  return (
    <Box role="menu">
      {attachRow}
      {items.map(({ key, label, submenu, Icon }) => (
        <Box
          key={key}
          role="menuitem"
          tabIndex={0}
          data-testid={`plus-menu-${key}`}
          sx={rowSx}
          onClick={(e) => submenu && onSelectSubmenu(submenu, e.currentTarget)}
          onMouseEnter={(e) => submenu && onSelectSubmenu(submenu, e.currentTarget)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              if (submenu) onSelectSubmenu(submenu, e.currentTarget);
            }
          }}
        >
          <Icon style={menuIconSx} />
          <Typography variant="labelMedium" sx={{ flex: 1 }}>{label}</Typography>
          <ArrowRightIcon style={menuIconSx} />
        </Box>
      ))}
    </Box>
  );
}

/**
 * The surface both the main menu and its submenu sit on — one component so
 * the two papers cannot drift apart in radius, border or elevation.
 */
export function MenuPaper({ children, fixedWidth = false }: { readonly children: ReactNode; readonly fixedWidth?: boolean }): ReactNode {
  return (
    <Paper
      elevation={8}
      data-testid={fixedWidth ? 'plus-submenu-paper' : 'plus-menu-paper'}
      sx={(theme: Theme) => ({
        minWidth: '17.5rem',
        // #6629: every submenu is the same width, whatever its rows hold.
        ...(fixedWidth ? { width: '17.5rem' } : {}),
        borderRadius: theme.vars.shape.radiusMd,
        border: '0.0625rem solid',
        borderColor: 'border.lines',
        background: theme.vars.palette.background.secondary,
        padding: 0,
        overflow: 'hidden',
      })}
    >
      {children}
    </Paper>
  );
}
