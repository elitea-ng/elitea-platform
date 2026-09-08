import type { ReactElement, ReactNode, SyntheticEvent } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { BaseTab } from '@/shared/ui/BaseTab';
import { BaseTabs } from '@/shared/ui/BaseTabs';
import { combineSx } from '@/shared/ui/lib/combineSx';

/** One tab in the header's left cluster (`StickyTabs.jsx:186-215`). */
export interface PageHeaderTab {
  readonly value: string;
  /** Rendered verbatim. A caller that shows a count folds it into this string. */
  readonly label: string;
  /** MUI's `Tab.icon` slot, which takes an element and not any `ReactNode`. */
  readonly icon?: ReactElement;
}

/** The tabs variant. When present, the tabs replace the title in the left cluster. */
export interface PageHeaderTabsConfig {
  readonly items: readonly PageHeaderTab[];
  /** `false` renders no tab as selected — MUI's own "nothing matched" value. */
  readonly selectedIndex?: number | false;
  readonly onChange?: (event: SyntheticEvent, index: number) => void;
  readonly ariaLabel?: string;
  /** Each tab gets `data-testid={`${testIdPrefix}-${tab.value}`}`. */
  readonly testIdPrefix?: string;
}

/** The right cluster, rendered in the reference's order. */
export interface PageHeaderSlots {
  readonly search?: ReactNode;
  readonly filters?: ReactNode;
  readonly viewToggle?: ReactNode;
  readonly actions?: ReactNode;
}

export interface PageHeaderProps {
  /** Title variant — `StickyTabs`' `showTitleAndSwitchBySelect` branch. Ignored when `tabs` is set. */
  readonly title?: string;
  readonly titleTestId?: string;
  /**
   * The element the title renders as. `div` is what the reference uses
   * (`StickyTabs.jsx:232`, `DrawerPageHeader.jsx:52`). A page whose only
   * heading is this row passes `h1`, so the page keeps one level-1 heading.
   */
  readonly titleComponent?: 'div' | 'h1' | 'h2';
  /** Item count, appended to the title as `Title (n)`. */
  readonly count?: number;
  readonly tabs?: PageHeaderTabsConfig;
  readonly slots?: PageHeaderSlots;
  /** `false` drops the bottom rule — `DrawerPageHeader`'s own `showBorder`. */
  readonly showBorder?: boolean;
  readonly sx?: SxProps<Theme>;
}

/**
 * The one page header every list page renders.
 *
 * MEASURED against the reference, which has no single header component: the
 * chrome is `components/StickyTabs.jsx` (the tab bar and its right-hand
 * action slot), and `[fsd]/features/settings/ui/drawer-page/
 * DrawerPageHeader.jsx` (the same bar with a title instead of tabs). This
 * widget is the union of the two, so one component covers both shapes.
 *
 * Layout and tokens come from `StickyTabs.jsx:60-95`:
 *  - the bar is `60px` tall, `flexShrink: 0`, with a bottom `divider` rule
 *    and `0 1.5rem` of horizontal padding;
 *  - the left cluster holds the tabs, or a `headingSmall` /
 *    `text.secondary` title (`StickyTabs.jsx:227-241`);
 *  - the right cluster (`MiddleArea`, `StickyTabs.jsx:76-86`) is
 *    `flex-end`-aligned, `35.5px` tall, `flexShrink: 0`, with a `20px` gap.
 *
 * The reference's search box is NOT in this bar: it lives in the right rail
 * (`components/RightPanel.jsx:67-86`), which `shared/ui/EntityRail` already
 * ports and `pages/skills` already uses. The `search` slot exists for the
 * pages whose search really is on the header row (credentials), and for the
 * narrow layout where the rail is hidden.
 */
export function PageHeader({
  title,
  titleTestId,
  titleComponent = 'div',
  count,
  tabs,
  slots,
  showBorder = true,
  sx,
}: PageHeaderProps): ReactNode {
  const { search, filters, viewToggle, actions } = slots ?? {};
  const hasActions = search !== undefined || filters !== undefined || viewToggle !== undefined || actions !== undefined;
  const titleText = count === undefined ? title : `${title ?? ''} (${count})`;

  return (
    <Box
      data-testid="page-header"
      sx={combineSx(headerSx(showBorder), sx)}
    >
      {tabs === undefined ? (
        <Typography
          variant="headingSmall"
          color="text.secondary"
          component={titleComponent}
          data-testid={titleTestId}
        >
          {titleText}
        </Typography>
      ) : (
        <BaseTabs
          value={tabs.selectedIndex ?? false}
          onChange={tabs.onChange}
          aria-label={tabs.ariaLabel}
        >
          {tabs.items.map((tab) => (
            <BaseTab
              key={tab.value}
              label={tab.label}
              icon={tab.icon}
              data-testid={tabs.testIdPrefix === undefined ? undefined : `${tabs.testIdPrefix}-${tab.value}`}
            />
          ))}
        </BaseTabs>
      )}
      {hasActions && (
        <Box sx={actionsSx}>
          {search}
          {filters}
          {viewToggle}
          {actions}
        </Box>
      )}
    </Box>
  );
}

/** `StickyTabs.jsx:60-74` (`FixedTabBar`). */
const headerSx =
  (showBorder: boolean): SxProps<Theme> =>
  () => ({
    flexShrink: 0,
    minHeight: '3.75rem',
    boxSizing: 'border-box',
    borderBottom: showBorder ? 1 : 0,
    borderColor: 'divider',
    padding: '0 1.5rem',
    display: 'flex',
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'space-between',
  });

/** `StickyTabs.jsx:76-86` (`MiddleArea`). */
const actionsSx: SxProps<Theme> = {
  flexGrow: 1,
  display: 'flex',
  boxSizing: 'border-box',
  justifyContent: 'flex-end',
  alignItems: 'center',
  minHeight: '2.21875rem',
  gap: '1.25rem',
  flexShrink: 0,
};
