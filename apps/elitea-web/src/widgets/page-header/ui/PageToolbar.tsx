import type { ReactElement, ReactNode, SyntheticEvent } from 'react';

import AddOutlinedIcon from '@mui/icons-material/AddOutlined';
import FilterListOutlinedIcon from '@mui/icons-material/FilterListOutlined';
import Badge from '@mui/material/Badge';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseTab } from '@/shared/ui/BaseTab';
import { BaseTabs } from '@/shared/ui/BaseTabs';
import { combineSx } from '@/shared/ui/lib/combineSx';
import { useSidePanelSlot } from '@/shared/ui/side-panel';

import { PageSearch, type PageSearchProps } from './PageSearch';

/** One tab in the toolbar's left cluster. */
export interface PageToolbarTab {
  readonly value: string;
  /** Rendered verbatim. A caller that shows a count folds it into this string. */
  readonly label: string;
  /** MUI's `Tab.icon` slot, which takes an element and not any `ReactNode`. */
  readonly icon?: ReactElement;
}

export interface PageToolbarTabsConfig {
  readonly items: readonly PageToolbarTab[];
  /** `false` renders no tab as selected — MUI's own "nothing matched" value. */
  readonly selectedIndex?: number | false;
  readonly onChange?: (event: SyntheticEvent, index: number) => void;
  readonly ariaLabel?: string;
  /** Each tab gets `data-testid={`${testIdPrefix}-${tab.value}`}`. */
  readonly testIdPrefix?: string;
}

/** The toolbar's one contained button: "+ New agent", "+ New credential"… */
export interface PageToolbarPrimaryAction {
  readonly label: string;
  readonly onClick: () => void;
  readonly testId?: string;
  readonly disabled?: boolean;
}

export interface PageToolbarProps {
  /** Left: the page's tabs. */
  readonly tabs?: PageToolbarTabsConfig;
  /** Left, when there are no tabs: a muted line such as "12 credentials". */
  readonly summary?: string | undefined;
  /** The page's search box, its text in the route's `query` param. */
  readonly search?: PageSearchProps | undefined;
  /** The Filters button, which opens the side panel's Filters tab; `activeCount` badges it. */
  readonly filters?: { readonly activeCount: number } | undefined;
  readonly viewToggle?: ReactNode;
  /** Outlined buttons (Import…), or an editor's own controls, before the primary action. */
  readonly secondaryActions?: ReactNode;
  /** Absent for a caller who may not create. */
  readonly primaryAction?: PageToolbarPrimaryAction | undefined;
  readonly sx?: SxProps<Theme>;
}

/**
 * The one toolbar row every page draws at the top of its body, under the
 * shell's title row — web and desktop alike (it is not in the desktop title
 * row, which is the window's drag region).
 *
 * The page's NAME is not here: the shell's breadcrumb carries it
 * (`shared/ui/page-chrome`). Left: the tabs, or a muted summary. Right, always
 * in this order: search, Filters, the view switch, secondary actions, the
 * primary "+ New X". A type gallery, with neither tabs nor a summary, leads
 * the row with its search instead. One height, one padding and one rule for every page, so
 * the controls sit in the same place wherever the person goes.
 */
export function PageToolbar({ tabs, summary, search, filters, viewToggle, secondaryActions, primaryAction, sx }: PageToolbarProps): ReactNode {
  // A type gallery has nothing else on the left: its search leads the row.
  const searchLeads = tabs === undefined && summary === undefined;
  const searchBox = search === undefined ? null : <PageSearch {...search} />;
  return (
    <Box
      data-testid="page-toolbar"
      sx={combineSx(toolbarSx, sx)}
    >
      <Box sx={leftSx}>
        {searchLeads && searchBox}
        {tabs === undefined ? (
          summary !== undefined && (
            <Typography
              variant="bodySmall"
              color="text.metrics"
              data-testid="page-toolbar-summary"
            >
              {summary}
            </Typography>
          )
        ) : (
          <PageToolbarTabBar tabs={tabs} />
        )}
      </Box>
      <Box sx={actionsSx}>
        {!searchLeads && searchBox}
        {filters !== undefined && <FiltersButton activeCount={filters.activeCount} />}
        {viewToggle}
        {secondaryActions}
        {primaryAction !== undefined && (
          <BaseBtn
            variant="contained"
            startIcon={<AddOutlinedIcon />}
            onClick={primaryAction.onClick}
            disabled={primaryAction.disabled ?? false}
            data-testid={primaryAction.testId}
            sx={noWrapSx}
          >
            {primaryAction.label}
          </BaseBtn>
        )}
      </Box>
    </Box>
  );
}

function PageToolbarTabBar({ tabs }: { readonly tabs: PageToolbarTabsConfig }): ReactNode {
  return (
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
  );
}

/** Opens the shell's right panel on Filters; absent where the shell has no panel. */
function FiltersButton({ activeCount }: { readonly activeCount: number }): ReactNode {
  const slot = useSidePanelSlot();
  if (slot === null) return null;
  return (
    <Badge
      color="primary"
      badgeContent={activeCount}
      invisible={activeCount === 0}
    >
      <BaseBtn
        variant="secondary"
        startIcon={<FilterListOutlinedIcon />}
        onClick={() => slot.show('filters')}
        data-testid="page-toolbar-filters"
        sx={noWrapSx}
      >
        {t('widgets.pageToolbar.filters', 'Filters')}
      </BaseBtn>
    </Badge>
  );
}

/** A page under the shell's title row: the toolbar, then the body. */
export const pageSx: SxProps<Theme> = { height: '100%', display: 'flex', flexDirection: 'column' };

/**
 * The page's body under its toolbar: it takes the rest of the height and
 * scrolls. Lists pad themselves to the toolbar's 1.5rem gutter
 * (`shared/ui/EntityCardList`), so the body adds no padding of its own.
 */
export const pageBodySx: SxProps<Theme> = { flex: 1, minHeight: 0, overflowY: 'auto', display: 'flex', flexDirection: 'column' };

const toolbarSx: SxProps<Theme> = {
  flexShrink: 0,
  minHeight: '3rem',
  boxSizing: 'border-box',
  borderBottom: 1,
  borderColor: 'divider',
  paddingInline: '1.5rem',
  display: 'flex',
  flexDirection: 'row',
  alignItems: 'center',
  justifyContent: 'space-between',
  gap: '1rem',
  containerType: 'inline-size',
  containerName: 'page-toolbar',
};

const leftSx: SxProps<Theme> = { minWidth: 0, display: 'flex', alignItems: 'center', alignSelf: 'stretch' };

const actionsSx: SxProps<Theme> = {
  flexGrow: 1,
  display: 'flex',
  justifyContent: 'flex-end',
  alignItems: 'center',
  gap: '0.75rem',
  flexShrink: 0,
  paddingBlock: '0.5rem',
};

const noWrapSx: SxProps<Theme> = { flexShrink: 0, whiteSpace: 'nowrap' };
