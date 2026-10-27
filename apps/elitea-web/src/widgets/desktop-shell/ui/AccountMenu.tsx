/**
 * The sidebar's foot: who is signed in, to which tenant, and the menu of
 * everything about the account and the app that is not a place to work in.
 *
 * The button reads "avatar · name · tenant ⌄" (the tenant is the deployment's
 * display name, `useHostInfo`), with a separate bug button beside it that
 * opens a prefilled GitHub issue (`lib/accountLinks.ts`). The menu opens
 * upwards: the email, Settings (⌘,), Usage, the Elitea items moved out of the
 * sidebar (`ACCOUNT_MENU_ITEMS`), Appearance and Language, help, what's new,
 * Troubleshoot and Sign out.
 *
 * A language change saves the choice and reloads: `t()` is not reactive
 * (`shared/i18n/locales.ts`), and the host keeps the session across a reload.
 */
import { useState, type ReactNode } from 'react';

import BugReportOutlinedIcon from '@mui/icons-material/BugReportOutlined';
import CheckIcon from '@mui/icons-material/Check';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import DataUsageOutlinedIcon from '@mui/icons-material/DataUsageOutlined';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import HelpOutlineOutlinedIcon from '@mui/icons-material/HelpOutlineOutlined';
import KeyOutlinedIcon from '@mui/icons-material/KeyOutlined';
import LanguageOutlinedIcon from '@mui/icons-material/LanguageOutlined';
import LogoutOutlinedIcon from '@mui/icons-material/LogoutOutlined';
import MedicalServicesOutlinedIcon from '@mui/icons-material/MedicalServicesOutlined';
import NewReleasesOutlinedIcon from '@mui/icons-material/NewReleasesOutlined';
import PaletteOutlinedIcon from '@mui/icons-material/PaletteOutlined';
import SettingsOutlinedIcon from '@mui/icons-material/SettingsOutlined';
import StorefrontOutlinedIcon from '@mui/icons-material/StorefrontOutlined';
import Avatar from '@mui/material/Avatar';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Divider from '@mui/material/Divider';
import IconButton from '@mui/material/IconButton';
import ListItemIcon from '@mui/material/ListItemIcon';
import ListItemText from '@mui/material/ListItemText';
import ListSubheader from '@mui/material/ListSubheader';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import { useColorScheme, useTheme, type Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { performLogout } from '@/shared/api/auth';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { LOCALES, currentLocale, saveLocale, t, type Locale } from '@/shared/i18n';
import { useIsUsageVisible } from '@/shared/lib/hooks/useIsUsageVisible';

import { bugReportUrl, whatsNewUrl } from '../lib/accountLinks';
import type { EliteaItem, EliteaItemValue } from '../lib/eliteaSections';
import type { ShellAction } from '../lib/shellActions';
import { useHostInfo } from '../model/useHostInfo';
import { modKey } from './shortcutLabel';

type ThemeMode = 'system' | 'light' | 'dark';

interface CurrentAuthor {
  name?: string;
  email?: string;
  avatar?: string;
}

const ITEM_ICONS: Partial<Record<EliteaItemValue, ReactNode>> = {
  credentials: <KeyOutlinedIcon fontSize="small" />,
  catalog: <StorefrontOutlinedIcon fontSize="small" />,
};

/** The sidebar's scale (`labelSmall`, 16px icons, compact rows), so the menu reads as part of it rather than as MUI's larger default. */
const MENU_LIST_SX = (theme: Theme) => ({
  '& .MuiMenuItem-root': { minHeight: '1.75rem', paddingY: 0.5, ...theme.typography.labelSmall },
  '& .MuiListItemText-primary': theme.typography.labelSmall,
  '& .MuiListItemIcon-root': { minWidth: '1.75rem' },
  '& .MuiListItemIcon-root .MuiSvgIcon-root, & .MuiMenuItem-root > .MuiSvgIcon-root': { fontSize: '1rem' },
  '& .MuiListSubheader-root': { ...theme.typography.labelSmall, lineHeight: '1.75rem' },
});

function initials(name: string): string {
  return name
    .split(/\s+/)
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase())
    .join('')
    .slice(0, 2);
}

interface Choice<T extends string> {
  readonly value: T;
  readonly label: string;
}

interface ChoiceSubmenuProps<T extends string> {
  readonly icon: ReactNode;
  readonly label: string;
  readonly value: T;
  readonly choices: readonly Choice<T>[];
  readonly onChoose: (value: T) => void;
  readonly testId: string;
}

/** A menu row that opens its choices beside it (click, or the arrow toward the end side), the current one checked. */
function ChoiceSubmenu<T extends string>({ icon, label, value, choices, onChoose, testId }: ChoiceSubmenuProps<T>): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  // Popover positions are physical: in a right-to-left window the end side is the left.
  const end = useTheme().direction === 'rtl' ? 'left' : 'right';
  const start = end === 'left' ? 'right' : 'left';
  return (
    <>
      <MenuItem
        data-testid={testId}
        aria-haspopup="menu"
        aria-expanded={anchor !== null}
        onClick={(event) => setAnchor(event.currentTarget)}
        onKeyDown={(event) => {
          if (event.key === (end === 'left' ? 'ArrowLeft' : 'ArrowRight')) setAnchor(event.currentTarget);
        }}
      >
        <ListItemIcon>{icon}</ListItemIcon>
        <ListItemText>{label}</ListItemText>
        <ChevronRightIcon fontSize="small" />
      </MenuItem>
      <Menu
        anchorEl={anchor}
        open={anchor !== null}
        onClose={() => setAnchor(null)}
        anchorOrigin={{ vertical: 'top', horizontal: end }}
        transformOrigin={{ vertical: 'top', horizontal: start }}
        slotProps={{ list: { dense: true, 'aria-label': label, sx: MENU_LIST_SX } }}
      >
        {choices.map((choice) => (
          <MenuItem
            key={choice.value}
            selected={choice.value === value}
            onClick={() => {
              setAnchor(null);
              onChoose(choice.value);
            }}
          >
            <ListItemIcon>{choice.value === value ? <CheckIcon fontSize="small" /> : null}</ListItemIcon>
            <ListItemText>{choice.label}</ListItemText>
          </MenuItem>
        ))}
      </Menu>
    </>
  );
}

function openExternally(url: string): void {
  // In the desktop app `window.open` hands http(s) URLs to the system browser (`shared/desktop/externalLinks.ts`).
  window.open(url, '_blank', 'noopener');
}

export interface AccountMenuProps {
  /** The Elitea items this person can open that live in this menu (`ACCOUNT_MENU_ITEMS`). */
  readonly items: readonly EliteaItem[];
  readonly run: (action: ShellAction) => void;
  /** The page on screen is one the menu leads to. */
  readonly selected?: boolean;
  /** Applies a new language (tests pass a spy). */
  readonly reload?: () => void;
}

interface AccountButtonProps {
  readonly author: CurrentAuthor;
  readonly tenant: string | undefined;
  readonly label: string;
  readonly expanded: boolean;
  /** On a page the menu leads to (Settings, Credentials, …): the sidebar's "you are here". */
  readonly selected: boolean;
  readonly onOpen: (anchor: HTMLElement) => void;
}

/** "avatar · name · tenant ⌄": the person, the deployment they are connected to, and the menu's disclosure. */
function AccountButton({ author, tenant, label, expanded, selected, onOpen }: AccountButtonProps): React.JSX.Element {
  // An SSO account may carry only an email.
  const name = author.name || author.email || '';
  return (
    <ButtonBase
      data-testid="shell-account"
      aria-haspopup="menu"
      aria-expanded={expanded}
      aria-label={label}
      onClick={(event) => onOpen(event.currentTarget)}
      sx={(theme: Theme) => ({
        flex: 1,
        minWidth: 0,
        justifyContent: 'flex-start',
        gap: 1,
        paddingX: 1,
        paddingY: 0.75,
        borderRadius: theme.vars.shape.radiusSm,
        color: theme.vars.palette.text.secondary,
        background: selected ? theme.vars.palette.background.button.drawerMenu.selected : 'transparent',
        '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
      })}
    >
      <Avatar src={author.avatar || undefined} alt={name} sx={{ width: '1.5rem', height: '1.5rem', fontSize: '0.6875rem' }}>
        {initials(name)}
      </Avatar>
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
    </ButtonBase>
  );
}

function reloadPage(): void {
  window.location.reload();
}

export function AccountMenu({ items, run, selected = false, reload = reloadPage }: AccountMenuProps): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const { data } = useGetCurrentAuthor();
  const author = (data?.data ?? {}) as CurrentAuthor;
  const { tenant, version } = useHostInfo();
  const usageVisible = useIsUsageVisible();
  const { mode, setMode } = useColorScheme();
  const close = (): void => setAnchor(null);
  const go = (to: string): void => {
    close();
    run({ type: 'go', to });
  };
  const help = items.find((item) => item.value === 'help');
  const reportBug = (): void => {
    close();
    openExternally(bugReportUrl({ version, userAgent: navigator.userAgent, locale: currentLocale() }));
  };
  const chooseTheme = (next: ThemeMode): void => {
    close();
    // As the Settings toggle does (`shared/ui/ThemeModeToggle.tsx`): applied at once, then stored for the user's other clients.
    setMode(next);
    void import('@/shared/api/themePreference')
      .then(({ saveThemePreference }) => saveThemePreference(next))
      .catch(() => undefined);
  };
  const chooseLanguage = (next: Locale): void => {
    close();
    if (next === currentLocale()) return;
    saveLocale(next);
    reload();
  };
  const menuLabel = t('desktop.account.menu', 'Account menu');
  const bugLabel = t('desktop.account.reportBug', 'Report a bug');

  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5 }}>
      <AccountButton author={author} tenant={tenant} label={menuLabel} expanded={anchor !== null} selected={selected} onOpen={setAnchor} />
      <Tooltip title={bugLabel}>
        <IconButton size="small" aria-label={bugLabel} data-testid="shell-report-bug" onClick={reportBug}>
          <BugReportOutlinedIcon fontSize="inherit" />
        </IconButton>
      </Tooltip>
      <Menu
        anchorEl={anchor}
        open={anchor !== null}
        onClose={close}
        anchorOrigin={{ vertical: 'top', horizontal: 'left' }}
        transformOrigin={{ vertical: 'bottom', horizontal: 'left' }}
        slotProps={{ list: { dense: true, 'aria-label': menuLabel, sx: MENU_LIST_SX }, paper: { sx: { minWidth: '15rem' } } }}
      >
        {author.email !== undefined && author.email !== '' && (
          // A subheader, not an item: the menu's keyboard focus skips it (`muiSkipListHighlight`).
          <ListSubheader
            disableSticky
            sx={(theme: Theme) => ({ background: 'transparent', color: theme.vars.palette.text.metrics, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' })}
          >
            {author.email}
          </ListSubheader>
        )}
        <MenuItem onClick={() => go('/settings')}>
          <ListItemIcon>
            <SettingsOutlinedIcon fontSize="small" />
          </ListItemIcon>
          <ListItemText>{t('desktop.shell.settings', 'Settings')}</ListItemText>
          <Typography variant="bodySmall" component="kbd" sx={{ fontFamily: 'inherit', marginLeft: 2 }}>
            {`${modKey()},`}
          </Typography>
        </MenuItem>
        {usageVisible && (
          <MenuItem onClick={() => go('/settings/usage')}>
            <ListItemIcon>
              <DataUsageOutlinedIcon fontSize="small" />
            </ListItemIcon>
            <ListItemText>{t('desktop.account.usage', 'Usage')}</ListItemText>
          </MenuItem>
        )}
        {items
          .filter((item) => item.value !== 'help')
          .map((item) => (
            <MenuItem key={item.value} data-testid={`shell-account-${item.value}`} onClick={() => go(item.url)}>
              <ListItemIcon>{ITEM_ICONS[item.value]}</ListItemIcon>
              <ListItemText>{item.label}</ListItemText>
            </MenuItem>
          ))}
        <Divider />
        <ChoiceSubmenu<ThemeMode>
          testId="shell-account-appearance"
          icon={<PaletteOutlinedIcon fontSize="small" />}
          label={t('desktop.account.appearance', 'Appearance')}
          value={mode ?? 'system'}
          choices={[
            { value: 'system', label: t('shared.ui.themeModeToggle.system', 'System') },
            { value: 'light', label: t('shared.ui.themeModeToggle.light', 'Light') },
            { value: 'dark', label: t('shared.ui.themeModeToggle.dark', 'Dark') },
          ]}
          onChoose={chooseTheme}
        />
        <ChoiceSubmenu<Locale>
          testId="shell-account-language"
          icon={<LanguageOutlinedIcon fontSize="small" />}
          label={t('desktop.account.language', 'Language')}
          value={currentLocale()}
          choices={LOCALES.map(({ code, label }) => ({ value: code, label }))}
          onChoose={chooseLanguage}
        />
        <Divider />
        {help !== undefined && (
          <MenuItem onClick={() => go(help.url)}>
            <ListItemIcon>
              <HelpOutlineOutlinedIcon fontSize="small" />
            </ListItemIcon>
            <ListItemText>{t('desktop.account.getHelp', 'Get help')}</ListItemText>
          </MenuItem>
        )}
        <MenuItem
          data-testid="shell-account-whats-new"
          onClick={() => {
            close();
            openExternally(whatsNewUrl(version));
          }}
        >
          <ListItemIcon>
            <NewReleasesOutlinedIcon fontSize="small" />
          </ListItemIcon>
          <ListItemText>{t('desktop.account.whatsNew', 'What’s new')}</ListItemText>
          {version !== undefined && (
            <Typography variant="bodySmall" sx={(theme: Theme) => ({ marginLeft: 2, color: theme.vars.palette.text.metrics })}>
              {version}
            </Typography>
          )}
        </MenuItem>
        <MenuItem onClick={() => go('/settings/troubleshoot')}>
          <ListItemIcon>
            <MedicalServicesOutlinedIcon fontSize="small" />
          </ListItemIcon>
          <ListItemText>{t('desktop.account.troubleshoot', 'Troubleshoot')}</ListItemText>
        </MenuItem>
        <Divider />
        <MenuItem
          data-testid="shell-account-sign-out"
          onClick={() => {
            close();
            performLogout();
          }}
        >
          <ListItemIcon>
            <LogoutOutlinedIcon fontSize="small" />
          </ListItemIcon>
          <ListItemText>{t('desktop.account.signOut', 'Sign out')}</ListItemText>
        </MenuItem>
      </Menu>
    </Box>
  );
}
