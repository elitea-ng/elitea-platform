/**
 * The account menu's frame, shared by the user app (web and desktop,
 * `AccountMenu.tsx`) and the admin console (`pages/admin/AdminNavFooter.tsx`):
 * the person at the sidebar's foot, and the menu that opens upward from them.
 *
 * The button reads "avatar · name · tenant ⌄"; on a collapsed rail it is the
 * avatar alone, named by a tooltip. The menu is an MUI `Menu` (`role=menu`,
 * arrow keys between rows, focus back on the button when it closes) laid out
 * top to bottom as: the email, the caller's rows (`children`), the version
 * line (a click copies it, for a support request) and Sign out, always last.
 */
import { useState, type ReactNode } from 'react';

import ContentCopyOutlinedIcon from '@mui/icons-material/ContentCopyOutlined';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import InfoOutlinedIcon from '@mui/icons-material/InfoOutlined';
import LogoutOutlinedIcon from '@mui/icons-material/LogoutOutlined';
import Avatar from '@mui/material/Avatar';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Menu from '@mui/material/Menu';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { handleCopy } from '@/shared/lib/clipboard';
import { MenuDivider, MenuHeader, MenuRow, MenuRowHint, MenuRowIcon, MenuRowText, pressToOpen } from '@/shared/ui/native-menu';

export interface AccountIdentity {
  readonly name?: string | undefined;
  readonly email?: string | undefined;
  readonly avatar?: string | undefined;
}

export interface AccountMenuFrameProps {
  readonly identity: AccountIdentity;
  /** The deployment's display name (desktop). */
  readonly tenant?: string | undefined;
  /** The rail is collapsed: the button is the avatar alone. */
  readonly collapsed?: boolean;
  /** The page on screen is one the menu leads to: the sidebar's "you are here". */
  readonly selected?: boolean;
  /** The version line, or nothing to leave it out. */
  readonly version?: string | undefined;
  /** Fires once the menu opens (a lazy read can wait for it). */
  readonly onOpen?: () => void;
  readonly onSignOut: () => void;
  /** Beside the button, e.g. the desktop's bug report. */
  readonly trailing?: ReactNode;
  readonly testIds?: { readonly button?: string; readonly signOut?: string };
  /**
   * The rows between the email and the version line; `close` closes the menu.
   * An array of keyed rows, never a Fragment: MUI's menu list moves focus
   * between its direct children.
   */
  readonly children: (close: () => void) => readonly ReactNode[];
}

function initials(name: string): string {
  return name
    .split(/\s+/)
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase())
    .join('')
    .slice(0, 2);
}

interface AccountButtonProps {
  readonly identity: AccountIdentity;
  readonly tenant: string | undefined;
  readonly collapsed: boolean;
  readonly label: string;
  readonly expanded: boolean;
  readonly selected: boolean;
  readonly testId: string;
  readonly onOpen: (anchor: HTMLElement) => void;
}

function AccountButton({ identity, tenant, collapsed, label, expanded, selected, testId, onOpen }: AccountButtonProps): React.JSX.Element {
  // An SSO account may carry only an email.
  const name = identity.name || identity.email || '';
  const button = (
    <ButtonBase
      data-testid={testId}
      aria-haspopup="menu"
      aria-expanded={expanded}
      aria-label={label}
      {...pressToOpen(onOpen)}
      sx={(theme: Theme) => ({
        flex: collapsed ? '0 0 auto' : 1,
        minWidth: 0,
        justifyContent: collapsed ? 'center' : 'flex-start',
        gap: 1,
        paddingX: collapsed ? 0.5 : 1,
        paddingY: 0.75,
        borderRadius: theme.vars.shape.radiusSm,
        color: theme.vars.palette.text.secondary,
        background: selected ? theme.vars.palette.background.button.drawerMenu.selected : 'transparent',
        '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
        '&.Mui-focusVisible': { outline: `2px solid ${theme.vars.palette.primary.main}`, outlineOffset: '-2px' },
      })}
    >
      <Avatar
        src={identity.avatar || undefined}
        alt={name}
        sx={(theme: Theme) => ({ width: '1.5rem', height: '1.5rem', fontSize: theme.typography.labelSmall.fontSize })}
      >
        {initials(name)}
      </Avatar>
      {!collapsed && (
        <>
          <Typography variant="labelSmall" component="span" noWrap sx={{ flex: 1, minWidth: 0, textAlign: 'left' }}>
            {name}
            {tenant !== undefined && (
              <Box
                component="span"
                data-testid="shell-account-tenant"
                sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics, '&::before': { content: '" · "' } })}
              >
                {tenant}
              </Box>
            )}
          </Typography>
          <ExpandMoreIcon sx={{ width: '1rem', height: '1rem', flexShrink: 0 }} />
        </>
      )}
    </ButtonBase>
  );
  return collapsed ? (
    <Tooltip title={name} placement="right">
      {button}
    </Tooltip>
  ) : (
    button
  );
}

const COPIED_FOR_MS = 1500;

/** The version line: a quiet row whose click copies it. */
function VersionRow({ version }: { readonly version: string }): React.JSX.Element {
  const [copied, setCopied] = useState(false);
  return (
    <MenuRow
      data-testid="account-menu-version"
      onClick={() => {
        void handleCopy(version).then(() => {
          setCopied(true);
          window.setTimeout(() => setCopied(false), COPIED_FOR_MS);
        });
      }}
      aria-label={`${version}. ${t('widgets.accountMenu.copyVersion', 'Copy')}`}
    >
      <MenuRowIcon>
        <InfoOutlinedIcon />
      </MenuRowIcon>
      <Typography
        variant="bodySmall"
        component="span"
        noWrap
        sx={(theme: Theme) => ({ flex: 1, minWidth: 0, color: theme.vars.palette.text.metrics })}
      >
        {version}
      </Typography>
      <MenuRowHint>{copied ? t('widgets.accountMenu.copied', 'Copied') : <ContentCopyOutlinedIcon sx={{ width: '0.875rem', height: '0.875rem', verticalAlign: 'middle' }} />}</MenuRowHint>
    </MenuRow>
  );
}

const DEFAULT_TEST_IDS = { button: 'shell-account', signOut: 'shell-account-sign-out' } as const;

export function AccountMenuFrame({
  identity,
  tenant,
  collapsed = false,
  selected = false,
  version,
  onOpen,
  onSignOut,
  trailing,
  testIds = DEFAULT_TEST_IDS,
  children,
}: AccountMenuFrameProps): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const close = (): void => setAnchor(null);
  const open = (next: HTMLElement): void => {
    setAnchor(next);
    onOpen?.();
  };
  const menuLabel = t('desktop.account.menu', 'Account menu');

  return (
    <Box sx={{ display: 'flex', alignItems: 'center', justifyContent: collapsed ? 'center' : undefined, gap: 0.5 }}>
      <AccountButton
        identity={identity}
        tenant={tenant}
        collapsed={collapsed}
        label={menuLabel}
        expanded={anchor !== null}
        selected={selected}
        testId={testIds.button ?? DEFAULT_TEST_IDS.button}
        onOpen={open}
      />
      {!collapsed && trailing}
      <Menu
        anchorEl={anchor}
        open={anchor !== null}
        onClose={close}
        anchorOrigin={{ vertical: 'top', horizontal: collapsed ? 'right' : 'left' }}
        transformOrigin={{ vertical: 'bottom', horizontal: 'left' }}
        slotProps={{ list: { dense: true, 'aria-label': menuLabel }, paper: { sx: { minWidth: '15rem' } } }}
      >
        {identity.email ? <MenuHeader>{identity.email}</MenuHeader> : null}
        {children(close)}
        <MenuDivider />
        {version !== undefined && <VersionRow version={version} />}
        <MenuRow
          data-testid={testIds.signOut ?? DEFAULT_TEST_IDS.signOut}
          onClick={() => {
            close();
            onSignOut();
          }}
        >
          <MenuRowIcon>
            <LogoutOutlinedIcon />
          </MenuRowIcon>
          <MenuRowText>{t('desktop.account.signOut', 'Sign out')}</MenuRowText>
        </MenuRow>
      </Menu>
    </Box>
  );
}
