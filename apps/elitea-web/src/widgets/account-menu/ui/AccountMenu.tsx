/**
 * The user app's account menu, the same on the web and in the desktop app:
 * who is signed in, and everything about the account and the app that is not
 * a place to work in (`AccountMenuFrame.tsx` draws the button and the menu).
 *
 * Rows, top to bottom: Settings, Usage, the account places moved out of the
 * sidebar (Credentials, Catalog), Admin portal for an administrator,
 * Appearance and Language, Get help, What's new, Troubleshoot (desktop), then
 * the version line and Sign out.
 *
 * The desktop adds the tenant on the button, the bug report button beside
 * it, the ⌘, hint and Troubleshoot, and opens the admin portal in the system
 * browser: the console runs on the browser session, not the app's token.
 *
 * A language change saves the choice and reloads: `t()` is not reactive
 * (`shared/i18n/locales.ts`), and the session survives a reload.
 */
import { useState, type ReactNode } from 'react';

import AdminPanelSettingsOutlinedIcon from '@mui/icons-material/AdminPanelSettingsOutlined';
import BugReportOutlinedIcon from '@mui/icons-material/BugReportOutlined';
import CheckIcon from '@mui/icons-material/Check';
import DataUsageOutlinedIcon from '@mui/icons-material/DataUsageOutlined';
import HelpOutlineOutlinedIcon from '@mui/icons-material/HelpOutlineOutlined';
import KeyOutlinedIcon from '@mui/icons-material/KeyOutlined';
import LanguageOutlinedIcon from '@mui/icons-material/LanguageOutlined';
import MedicalServicesOutlinedIcon from '@mui/icons-material/MedicalServicesOutlined';
import NewReleasesOutlinedIcon from '@mui/icons-material/NewReleasesOutlined';
import OpenInNewIcon from '@mui/icons-material/OpenInNew';
import PaletteOutlinedIcon from '@mui/icons-material/PaletteOutlined';
import SettingsOutlinedIcon from '@mui/icons-material/SettingsOutlined';
import StorefrontOutlinedIcon from '@mui/icons-material/StorefrontOutlined';
import IconButton from '@mui/material/IconButton';
import { useColorScheme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';

import { performLogout } from '@/shared/api/auth';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { appVersion } from '@/shared/config/appVersion';
import { LOCALES, currentLocale, saveLocale, t, type Locale } from '@/shared/i18n';
import { useIsUsageVisible } from '@/shared/lib/hooks/useIsUsageVisible';
import { MenuDivider, MenuRow, MenuRowHint, MenuRowIcon, MenuRowText, NativeSubmenu } from '@/shared/ui/native-menu';

import { bugReportUrl, whatsNewUrl } from '../lib/accountLinks';
import { adminPortalUrl } from '../lib/adminPortal';
import { versionLine } from '../lib/versionLine';
import { useAdminAccess } from '../model/useAdminAccess';
import { useHostInfo } from '../model/useHostInfo';
import { useServerVersion } from '../model/useServerVersion';
import { AccountMenuFrame, type AccountIdentity } from './AccountMenuFrame';

type ThemeMode = 'system' | 'light' | 'dark';

/** A place that lives in this menu rather than in the sidebar. */
export interface AccountMenuItem {
  readonly value: 'credentials' | 'catalog' | 'help' | (string & {});
  readonly label: string;
  readonly url: string;
}

const ITEM_ICONS: Partial<Record<string, ReactNode>> = {
  credentials: <KeyOutlinedIcon />,
  catalog: <StorefrontOutlinedIcon />,
};

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

/** A menu row that opens its choices beside it (on hover, a click, or the arrow toward the end side), the current one checked. */
function ChoiceSubmenu<T extends string>({ icon, label, value, choices, onChoose, testId }: ChoiceSubmenuProps<T>): React.JSX.Element {
  return (
    <NativeSubmenu testId={testId} icon={icon} label={label} minWidth="10rem">
      {(close) =>
        choices.map((choice) => (
          <MenuRow
            key={choice.value}
            role="menuitemradio"
            aria-checked={choice.value === value}
            onClick={() => {
              close();
              onChoose(choice.value);
            }}
          >
            <MenuRowIcon>{choice.value === value ? <CheckIcon /> : null}</MenuRowIcon>
            <MenuRowText>{choice.label}</MenuRowText>
          </MenuRow>
        ))
      }
    </NativeSubmenu>
  );
}

function openExternally(url: string): void {
  // In the desktop app `window.open` hands http(s) URLs to the system browser (`shared/desktop/externalLinks.ts`).
  window.open(url, '_blank', 'noopener');
}

function reloadPage(): void {
  window.location.reload();
}

export interface AccountMenuProps {
  /** The places this person can open that live in this menu (Credentials, Catalog, Help). */
  readonly items: readonly AccountMenuItem[];
  /** Goes to an app path. */
  readonly go: (to: string) => void;
  readonly platform: 'web' | 'desktop';
  /** The web rail is collapsed: the button is the avatar alone. */
  readonly collapsed?: boolean;
  /** The page on screen is one the menu leads to. */
  readonly selected?: boolean;
  /** The Settings shortcut to show beside Settings (the desktop's ⌘,). */
  readonly settingsShortcut?: string | undefined;
  /** Applies a new language (tests pass a spy). */
  readonly reload?: () => void;
}

export function AccountMenu({ items, go, platform, collapsed = false, selected = false, settingsShortcut, reload = reloadPage }: AccountMenuProps): React.JSX.Element {
  const desktop = platform === 'desktop';
  const [opened, setOpened] = useState(false);
  const { data } = useGetCurrentAuthor();
  const identity = (data?.data ?? {}) as AccountIdentity;
  const host = useHostInfo();
  const usageVisible = useIsUsageVisible();
  const admin = useAdminAccess();
  const serverVersion = useServerVersion(opened);
  const { mode, setMode } = useColorScheme();
  // The desktop's own version is the app's (`host_state`); the web's is the build's.
  const clientVersion = desktop ? host.version : appVersion();
  const version =
    clientVersion === undefined
      ? undefined
      : versionLine(clientVersion, serverVersion, {
          client: t('widgets.accountMenu.version', 'Version'),
          server: t('widgets.accountMenu.server', 'Server'),
        });
  const help = items.find((item) => item.value === 'help');
  const helpLabel = t('desktop.account.getHelp', 'Get help');
  const bugLabel = t('desktop.account.reportBug', 'Report a bug');

  const chooseTheme = (next: ThemeMode): void => {
    // As the Settings toggle does (`shared/ui/ThemeModeToggle.tsx`): applied at once, then stored for the user's other clients.
    setMode(next);
    void import('@/shared/api/themePreference')
      .then(({ saveThemePreference }) => saveThemePreference(next))
      .catch(() => undefined);
  };
  const chooseLanguage = (next: Locale): void => {
    if (next === currentLocale()) return;
    saveLocale(next);
    reload();
  };

  const trailing = desktop ? (
    <Tooltip title={bugLabel}>
      <IconButton
        size="small"
        aria-label={bugLabel}
        data-testid="shell-report-bug"
        onClick={() => openExternally(bugReportUrl({ version: host.version, userAgent: navigator.userAgent, locale: currentLocale() }))}
      >
        <BugReportOutlinedIcon fontSize="inherit" />
      </IconButton>
    </Tooltip>
  ) : undefined;

  return (
    <AccountMenuFrame
      identity={identity}
      tenant={host.tenant}
      collapsed={collapsed}
      selected={selected}
      version={version}
      onOpen={() => setOpened(true)}
      onSignOut={() => void performLogout()}
      trailing={trailing}
    >
      {(close) => {
        const goTo = (to: string): void => {
          close();
          go(to);
        };
        const rows: ReactNode[] = [
          <MenuRow key="settings" data-testid="shell-account-settings" onClick={() => goTo('/settings')}>
            <MenuRowIcon>
              <SettingsOutlinedIcon />
            </MenuRowIcon>
            <MenuRowText>{t('desktop.shell.settings', 'Settings')}</MenuRowText>
            {settingsShortcut !== undefined && <MenuRowHint component="kbd">{settingsShortcut}</MenuRowHint>}
          </MenuRow>,
        ];
        if (usageVisible) {
          rows.push(
            <MenuRow key="usage" onClick={() => goTo('/settings/usage')}>
              <MenuRowIcon>
                <DataUsageOutlinedIcon />
              </MenuRowIcon>
              <MenuRowText>{t('desktop.account.usage', 'Usage')}</MenuRowText>
            </MenuRow>,
          );
        }
        for (const item of items) {
          if (item.value === 'help') continue;
          rows.push(
            <MenuRow key={item.value} data-testid={`shell-account-${item.value}`} onClick={() => goTo(item.url)}>
              <MenuRowIcon>{ITEM_ICONS[item.value]}</MenuRowIcon>
              <MenuRowText>{item.label}</MenuRowText>
            </MenuRow>,
          );
        }
        if (admin) {
          const url = adminPortalUrl(desktop ? host.origin : undefined);
          rows.push(
            <MenuDivider key="admin-divider" />,
            // A real link on the web, so a middle click or ⌘-click opens a tab;
            // the desktop hands it to the system browser.
            <MenuRow
              key="admin"
              data-testid="shell-account-admin-portal"
              component={desktop ? undefined : 'a'}
              href={desktop ? undefined : url}
              onClick={() => {
                close();
                if (desktop) openExternally(url);
              }}
            >
              <MenuRowIcon>
                <AdminPanelSettingsOutlinedIcon />
              </MenuRowIcon>
              <MenuRowText>{t('widgets.accountMenu.adminPortal', 'Admin portal')}</MenuRowText>
              <MenuRowHint>
                <OpenInNewIcon sx={{ width: '0.875rem', height: '0.875rem', verticalAlign: 'middle' }} />
              </MenuRowHint>
            </MenuRow>,
          );
        }
        rows.push(
          <MenuDivider key="preferences-divider" />,
          <ChoiceSubmenu<ThemeMode>
            key="appearance"
            testId="shell-account-appearance"
            icon={<PaletteOutlinedIcon />}
            label={t('desktop.account.appearance', 'Appearance')}
            value={mode ?? 'system'}
            choices={[
              { value: 'system', label: t('shared.ui.themeModeToggle.system', 'System') },
              { value: 'light', label: t('shared.ui.themeModeToggle.light', 'Light') },
              { value: 'dark', label: t('shared.ui.themeModeToggle.dark', 'Dark') },
            ]}
            onChoose={(next) => {
              close();
              chooseTheme(next);
            }}
          />,
          <ChoiceSubmenu<Locale>
            key="language"
            testId="shell-account-language"
            icon={<LanguageOutlinedIcon />}
            label={t('desktop.account.language', 'Language')}
            value={currentLocale()}
            choices={LOCALES.map(({ code, label }) => ({ value: code, label }))}
            onChoose={(next) => {
              close();
              chooseLanguage(next);
            }}
          />,
          <MenuDivider key="help-divider" />,
        );
        if (help !== undefined) {
          rows.push(
            <MenuRow key="help" data-testid="shell-account-help" onClick={() => goTo(help.url)}>
              <MenuRowIcon>
                <HelpOutlineOutlinedIcon />
              </MenuRowIcon>
              <MenuRowText>{helpLabel}</MenuRowText>
            </MenuRow>,
          );
        }
        rows.push(
          <MenuRow
            key="whats-new"
            data-testid="shell-account-whats-new"
            onClick={() => {
              close();
              openExternally(whatsNewUrl(clientVersion));
            }}
          >
            <MenuRowIcon>
              <NewReleasesOutlinedIcon />
            </MenuRowIcon>
            <MenuRowText>{t('desktop.account.whatsNew', 'What’s new')}</MenuRowText>
          </MenuRow>,
        );
        if (desktop) {
          rows.push(
            <MenuRow key="troubleshoot" onClick={() => goTo('/settings/troubleshoot')}>
              <MenuRowIcon>
                <MedicalServicesOutlinedIcon />
              </MenuRowIcon>
              <MenuRowText>{t('desktop.account.troubleshoot', 'Troubleshoot')}</MenuRowText>
            </MenuRow>,
          );
        }
        return rows;
      }}
    </AccountMenuFrame>
  );
}
