import { memo, useCallback, useMemo } from 'react';

import { useLocation } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import Divider from '@mui/material/Divider';
import SvgIcon from '@mui/material/SvgIcon';
import type { SxProps, Theme } from '@mui/material/styles';

import { AnalyticsIcon } from '../icons/analytics-icon';
import { BellIcon } from '../icons/bell-icon';
import { BriefcaseIcon } from '../icons/briefcase-icon';
import { ConfigurationIcon } from '../icons/configuration-icon';
import { DialIcon } from '../icons/dial-icon';
import { EnvironmentIcon } from '../icons/environment-icon';
import { HumanIcon } from '../icons/human-icon';
import { KeyIcon } from '../icons/key-icon';
import { LockIcon } from '../icons/lock-icon';
import { LogoutIcon } from '../icons/logout-icon';
import { MemoryIcon } from '../icons/memory-icon';
import { ModelIcon } from '../icons/model-icon';
import { PersonalizationIcon } from '../icons/personalization-icon';
import { PromptIcon } from '../icons/prompt-icon';
import { ReasonIcon } from '../icons/reason-icon';

import { SETTINGS_LAYOUT } from './settings.constants';
import { t } from '@/shared/i18n';

/** Tab definition used by `SettingsDrawer`. */
export interface SettingsTab {
  /** Unique identifier — matches the URL path segment. */
  id: string;
  /** Display label for the tab. */
  label: string;
  /** Optional icon reference — SettingsDrawer resolves icons internally via ICON_COMPONENTS. */
  icon?: React.ComponentType;
}

/** A group of tabs under a section header. */
export interface SettingsSection {
  /** Section label shown above its tabs. */
  section: string;
  /** Tabs in this section. */
  tabs: SettingsTab[];
}

export interface SettingsDrawerProps {
  /** Sections with their tabs. */
  sections: SettingsSection[];
  /** Called when a tab item is clicked. */
  onItemClick?: (tabId: string) => void;
}

/*
 * ONE ENTRY PER TAB ID THAT `settings-layout.tsx` ACTUALLY RENDERS.
 *
 * This map used to be keyed on ids the layout no longer emits, so
 * `getIconComponent`'s `?? ConfigurationIcon` fallback answered for
 * `preferences`, `ai-personality`, `memory`, `usage` and `profile` — five of
 * the twelve nav rows drew the same generic gear. The baseline
 * (`[fsd]/features/settings/ui/settings-drawer/SettingsDrawer.jsx:26-42`)
 * gives each row its own glyph; these are that map, resolved against this
 * app's own icon set.
 */
const ICON_COMPONENTS: Record<string, React.ComponentType> = {
  'model-configuration': ModelIcon,
  prompts: PromptIcon,
  environment: EnvironmentIcon,
  tokens: KeyIcon,
  'project-params': BriefcaseIcon,
  secrets: LockIcon,
  users: HumanIcon,
  analytics: AnalyticsIcon,
  usage: DialIcon,
  profile: HumanIcon,
  personalization: PersonalizationIcon,
  preferences: PersonalizationIcon,
  'ai-personality': ReasonIcon,
  memory: MemoryIcon,
  notifications: BellIcon,
  logout: LogoutIcon,
};

const getIconComponent = (tabId: string): React.ComponentType => {
  return ICON_COMPONENTS[tabId] ?? ConfigurationIcon;
};

const menuItemSx =
  (isActive: boolean): SxProps<Theme> =>
  (theme) => ({
    // The item renders as a real <button> (see the render below). The four
    // properties here reset the user-agent button styling. That styling
    // would otherwise override the ported look: a grey background, a
    // bevelled border, the browser's own font, and centred text.
    border: 'none',
    font: 'inherit',
    textAlign: 'left',
    width: 'calc(100% - 1.5rem)',
    padding: '0.5rem 0.75rem',
    margin: '0 0.75rem',
    gap: '0.5rem',
    display: 'flex',
    alignItems: 'center',
    maxWidth: 'calc(100% - 1.5rem)',
    height: '2rem',
    background: isActive
      ? theme.vars.palette.background.userInputBackgroundActive
      : theme.vars.palette.background.conversation.normal,
    // oxlint-disable-next-line elitea/ad-hoc-radius — ported from baseline
    borderRadius: '0.375rem',
    cursor: 'pointer',
    transition: 'all 0.2s ease-in-out',
    boxSizing: 'border-box',
    '&:hover': {
      backgroundColor: theme.vars.palette.background.conversation.hover,
    },
  });

const iconWrapperSx =
  (isActive: boolean): SxProps<Theme> =>
  (theme) => ({
    display: 'flex',
    alignItems: 'center',
    minWidth: '1rem',
    color: isActive ? theme.vars.palette.text.secondary : theme.vars.palette.icon.fill.stateButtonHover,
    '& svg': {
      fill: isActive ? theme.vars.palette.text.secondary : theme.vars.palette.icon.fill.stateButtonHover,
      width: '1rem',
      height: '1rem',
    },
  });

const menuItemTextSx =
  (isActive: boolean): SxProps<Theme> =>
  (theme) => ({
    fontWeight: 500,
    // oxlint-disable-next-line elitea/ad-hoc-font-size — ported from baseline
    fontSize: '0.75rem',
    lineHeight: '1rem',
    letterSpacing: 0,
    color: isActive ? theme.vars.palette.text.secondary : theme.vars.palette.text.metrics,
  });

/**
 * Left sidebar navigation for the Settings page. Ported from
 * `apps/elitea-ui/src/[fsd]/features/settings/ui/settings-drawer/SettingsDrawer.jsx`.
 */
export const SettingsDrawer = memo(function SettingsDrawer({ sections, onItemClick }: SettingsDrawerProps) {
  const location = useLocation();

  const isActiveTab = useCallback(
    (tabId: string) => {
      if (!tabId) return false;

      const pathSegments = location.pathname.split('/');
      const lastSegment = pathSegments[pathSegments.length - 1];

      if (
        tabId === 'model-configuration' &&
        (lastSegment === 'create-configuration' || pathSegments[pathSegments.length - 2] === 'create-configuration')
      ) {
        return true;
      }
      if (tabId === 'tokens' && lastSegment === 'create-personal-token') {
        return true;
      }
      return lastSegment === tabId;
    },
    [location.pathname],
  );

  const handleItemClick = useCallback(
    (tabId: string) => {
      onItemClick?.(tabId);
    },
    [onItemClick],
  );

  const renderedSections = useMemo(
    () =>
      sections.map((section, sectionIndex) => (
        <Box
          key={section.section}
          sx={sectionGroupSx}
        >
          {sectionIndex > 0 && <Divider sx={sectionDividerSx} />}
          <Box
            component="span"
            sx={sectionHeaderSx}
          >
            {section.section}
          </Box>
          {section.tabs.map((tab) => {
            const IconComponent = getIconComponent(tab.id);
            const isActive = isActiveTab(tab.id);
            return (
              // A REAL BUTTON, NOT A DIV WITH onClick. Every item here — the
              // whole of Settings navigation, "Log out" included — used to be
              // a plain <Box onClick>. That renders a <div> with no role, no
              // tabindex and no href. A keyboard could not reach any of them
              // and assistive technology read the labels as plain text. The
              // accessibility tree of a live deployment showed all eleven as
              // StaticText.
              <Box
                key={tab.id}
                component="button"
                type="button"
                aria-current={isActive ? 'page' : undefined}
                onClick={() => handleItemClick(tab.id)}
                sx={menuItemSx(isActive)}
              >
                <Box sx={iconWrapperSx(isActive)}>
                  <SvgIcon
                    component={IconComponent}
                    inheritViewBox
                    sx={{ width: '1rem', height: '1rem' }}
                  />
                </Box>
                <Box
                  component="span"
                  sx={menuItemTextSx(isActive)}
                >
                  {tab.label}
                </Box>
              </Box>
            );
          })}
        </Box>
      )),
    [sections, isActiveTab, handleItemClick],
  );

  return (
    <Box sx={drawerSx}>
      <Box sx={headerSx}>
        <Box
          component="span"
          sx={headerTextSx}
        >
          {t('shared.ui.settings.drawer.title', 'Settings')}
        </Box>
      </Box>

      <Box sx={menuContainerSx}>{renderedSections}</Box>
    </Box>
  );
});

/** @type {MuiSx} */
const drawerSx: SxProps<Theme> = (theme) => ({
  width: SETTINGS_LAYOUT.DRAWER_WIDTH,
  minWidth: SETTINGS_LAYOUT.DRAWER_WIDTH,
  maxWidth: SETTINGS_LAYOUT.DRAWER_WIDTH,
  borderRight: `0.0625rem solid ${theme.vars.palette.border.table ?? 'transparent'}`,
  backgroundColor: theme.vars.palette.background.tabPanel,
  display: 'flex',
  flexDirection: 'column',
  height: '100%',
  boxSizing: 'border-box',
});

const headerSx: SxProps<Theme> = (theme) => ({
  padding: '1rem 1rem 1.1875rem 1.5rem',
  borderBottom: `0.0625rem solid ${theme.vars.palette.border.table ?? 'transparent'}`,
});

const headerTextSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.text.secondary,
  // oxlint-disable-next-line elitea/ad-hoc-font-size — ported from baseline
  fontSize: '1rem',
  fontWeight: 500,
});

const menuContainerSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  boxSizing: 'border-box',
  maxWidth: '100%',
  overflow: 'auto',
};

const sectionGroupSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.5rem',
  // Baseline `SettingsDrawer.jsx:180`. Without it PERSONAL's last row sat
  // flush against the bottom of the drawer and the two groups read as one.
  marginBottom: '1rem',
};

const sectionHeaderSx: SxProps<Theme> = (theme) => ({
  display: 'block',
  color: theme.vars.palette.text.metrics,
  fontWeight: 500,
  // oxlint-disable-next-line elitea/ad-hoc-font-size — ported from baseline
  fontSize: '0.75rem',
  lineHeight: '1rem',
  letterSpacing: '0.06em',
  textTransform: 'uppercase',
  // Baseline `SettingsDrawer.jsx:190`: the group label lines up with the
  // "Settings" title above it (both 1.5rem from the drawer's left edge), not
  // with the nav rows' 1rem.
  padding: '1rem 1rem 1rem 1.5rem',
});

const sectionDividerSx: SxProps<Theme> = (theme) => ({
  borderColor: theme.vars.palette.border.table,
  margin: 0,
});
