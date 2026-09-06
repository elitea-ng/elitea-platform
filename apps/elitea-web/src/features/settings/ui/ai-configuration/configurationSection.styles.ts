/**
 * The style map shared by `ConfigurationSection` and the two pieces split out
 * of it (`ConfigCards`, `DefaultSettingsSelects`).
 *
 * It lives in its OWN module rather than in `ConfigurationSection.tsx`
 * because both children need the type: importing it back from their parent
 * closed an import cycle, which `check-layer-cycle` fails.
 */
import type { Theme, useTheme } from '@mui/material/styles';

export function getStyles(theme: ReturnType<typeof useTheme>) {
  const t = theme as Theme;
  return {
    container: {
      padding: '0.5rem 1.5rem',
      gap: '0.5rem',
      display: 'flex',
      flexDirection: 'column',
      width: '100%',
      boxSizing: 'border-box' as const,
    },
    title: {
      color: t.vars.palette.text.secondary,
    },
    /** A ROW, not a column — baseline `defaultSettingsLayout="inline"`. */
    defaultSettingsContainer: {
      display: 'flex',
      flexDirection: 'row' as const,
      flexWrap: 'wrap' as const,
      alignItems: 'center',
      justifyContent: 'flex-start',
      gap: '1rem',
    },
    /** Baseline `labelContainerSx`, measured on production at 4px/12px, 12px radius. */
    defaultSettingPill: {
      display: 'flex',
      flexDirection: 'row' as const,
      alignItems: 'center',
      gap: '0.5rem',
      padding: '0.25rem 0.75rem',
      // oxlint-disable-next-line elitea/ad-hoc-radius -- baseline literal; the radius tokens are 4/8/16px and this pill is 12px.
      borderRadius: '0.75rem',
      border: `0.0625rem solid ${t.vars.palette.border.cardsOutlines}`,
      boxSizing: 'border-box' as const,
      // The label is one line. Left to wrap, "High-tier" broke after the
      // hyphen and the pill grew to 48px tall next to its 40px neighbours.
      whiteSpace: 'nowrap' as const,
      // The pill IS the field's outline; the select inside it must not draw a
      // second one underneath. `SingleSelect` has no `disableUnderline`
      // passthrough, so the standard variant's `::before`/`::after` rules are
      // switched off here.
      // oxlint-disable-next-line elitea/no-mui-internal-selector -- no prop on `shared/ui`'s SingleSelect reaches the underline pseudo-elements.
      '& .MuiInput-root::before, & .MuiInput-root::after': { borderBottom: 'none' },
    },
    defaultSettingSelect: {
      margin: 0,
      minWidth: '2.5rem',
    },
    configurationsContainer: {
      display: 'flex',
      flexDirection: 'row' as const,
      flexWrap: 'wrap' as const,
      gap: '1rem',
      justifyContent: 'flex-start',
      marginTop: '1rem',
    },
    groupContainer: (showBorder: boolean) => ({
      display: 'flex',
      flexDirection: 'column' as const,
      paddingTop: '0.5rem',
      paddingBottom: '1rem',
      borderBottom: showBorder ? `0.0625rem solid ${t.vars.palette.border.sidebarDivider}` : 'none',
    }),
    groupLabel: {
      textTransform: 'uppercase' as const,
    },
  };
}

export type ConfigurationSectionStyles = ReturnType<typeof getStyles>;
