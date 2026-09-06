/**
 * ThemeModeToggle — promoted from `apps/elitea-ui/src/components/ThemeModeToggle.jsx`
 * (Wave-2 unit A13, issue #26).
 *
 * In the new app theme state lives in MUI's `useColorScheme` hook (wired up by
 * `BrandThemeProvider` with `modeStorageKey: 'el-mode'`). This component
 * replaces the baseline's Redux-based `switchMode` action with direct calls to
 * `setColorScheme`.
 *
 * Consumed by: Unit A9's `UserSettings` (hence promoted to `shared/ui` rather
 * than left in `pages/mode-switch`).
 *
 * Deviations from baseline:
 *  - Uses `useColorScheme().setMode` instead of Redux `dispatch(actions.switchMode())`.
 *  - Uses `TabGroupButton` with the new `items` prop instead of `arrayBtn`.
 *  - Uses `shared/ui/icons` for MoonIcon/SunIcon (already ported in Wave-1/S2).
 *  - Uses `shared/lib/enums`'s `ThemeModeOptions` (already ported in Wave-1/S3).
 *  - `displayName` preserved for React DevTools parity.
 */
import { memo, useCallback, useMemo } from 'react';

import { useColorScheme } from '@mui/material/styles';

import { ThemeModeOptions } from '@/shared/lib/enums';
import { t } from '@/shared/i18n';
import { TabGroupButton } from '@/shared/ui/TabGroupButton';
import type { TabGroupButtonItem } from '@/shared/ui/TabGroupButton';
import { ContrastIcon } from '@/shared/ui/icons/contrast-icon';
import { MoonIcon } from '@/shared/ui/icons/moon-icon';
import { SunIcon } from '@/shared/ui/icons/sun-icon';

/**
 * Theme mode values used by this toggle.
 *
 * `'system'` is not in `shared/lib/enums`'s `ThemeModeOptions` (whose two
 * members are pinned by `enums.test.ts` as a port of the baseline's Redux
 * enum) because it is not a COLOUR SCHEME — it is the absence of a chosen
 * one. MUI models exactly that distinction: `setColorScheme` picks a scheme,
 * `setMode` may additionally be given `'system'`, which follows
 * `prefers-color-scheme` instead. The toggle therefore drives `mode`, not
 * `colorScheme`.
 */
type ThemeModeValue = 'system' | 'dark' | 'light';

const SYSTEM_MODE = 'system';

/**
 * A small MUI `ToggleButtonGroup`-backed control that lets the user switch
 * between the system, dark and light colour schemes.
 *
 * THREE buttons, and the click carries the value. This used to be two
 * (Dark / Light) with an `onChange` that ignored its argument and simply
 * flipped to "the other one" — so clicking the ALREADY-selected button
 * switched away from it, and "System", which production shows first and
 * which is MUI's own default (`BrandThemeProvider` mounts with
 * `defaultMode: 'system'`), could not be reached or even seen once a scheme
 * had been chosen.
 */
const ThemeModeToggle = memo(() => {
  const { mode, setMode } = useColorScheme();

  const onChange = useCallback(
    (value: string) => {
      setMode(value as ThemeModeValue);
    },
    [setMode],
  );

  const items = useMemo<TabGroupButtonItem[]>(() => [
    {
      value: SYSTEM_MODE,
      icon: <ContrastIcon />,
      label: t('shared.ui.themeModeToggle.system', 'System'),
      tooltip: t('shared.ui.themeModeToggle.systemTooltip', 'Follow the system theme'),
    },
    {
      value: ThemeModeOptions.Dark,
      icon: <MoonIcon />,
      label: t('shared.ui.themeModeToggle.dark', 'Dark'),
      tooltip: t('shared.ui.themeModeToggle.darkTooltip', 'Dark theme'),
    },
    {
      value: ThemeModeOptions.Light,
      icon: <SunIcon />,
      label: t('shared.ui.themeModeToggle.light', 'Light'),
      tooltip: t('shared.ui.themeModeToggle.lightTooltip', 'Light theme'),
    },
  ], []);

  return (
    <TabGroupButton
      items={items}
      value={mode ?? SYSTEM_MODE}
      onChange={onChange}
      size="small"
      ariaLabel={t('shared.ui.themeModeToggle.ariaLabel', 'Theme')}
      sx={themeModeToggleSx}
    />
  );
});

/** Production renders each of the three at a fixed 6.25rem so the group reads
 * as one segmented control rather than three differently-sized pills. */
const themeModeToggleSx = {
  // oxlint-disable-next-line elitea/no-mui-internal-selector -- this file is already scoped out of R-T6 in .oxlintrc.json for exactly this control.
  '& .MuiToggleButton-root': { minWidth: '6.25rem' },
};

ThemeModeToggle.displayName = 'ThemeModeToggle';

export default ThemeModeToggle;
