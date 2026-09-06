import type { ReactNode } from 'react';
import { useEffect, useState } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { combineSx } from '../lib/combineSx';

/**
 * The right-hand rail of every entity-list page.
 *
 * Ported from `apps/elitea-ui/src/components/RightPanel.jsx:22-82` (the
 * `FixedGrid` + `ContainerBox` pair) and the geometry constants it reads
 * (`common/constants.js:502-511`: `RIGHT_PANEL_WIDTH_OF_CARD_LIST_PAGE =
 * 328`, `PAGE_PADDING = 12`, `RIGHT_PANEL_HEIGHT_OFFSET = '16px'`,
 * `CARD_LIST_WIDTH = calc(100% - 328px)`), plus the collapse thresholds from
 * `hooks/useShouldCollapseRightToolbar.js:16-19`
 * (`DETAILS_PAGE_COLLAPSE_THRESHOLD = 700` /
 * `..._WITH_SIDEBAR_OPEN = 800`).
 *
 * **Why `shared/ui` and not `widgets/` or a `features/` slice.** The rail
 * has two classes of caller: `pages/**` (agents, pipelines, skills,
 * user-public) AND `features/toolkits`' own `ToolkitsList`, which already
 * renders a rail of its own and has an unfilled `rightPanelExtra` slot.
 * `.dependency-cruiser.cjs`'s `no-upward-from-features` rule forbids
 * `features/` importing `widgets/` or `pages/`, and `no-sideways-features`
 * forbids reaching into another feature slice — so a `widgets/` home would
 * be unreachable from the one caller that already exists. `entities/` is
 * likewise out: the rail needs BOTH the tag and the author domain, and
 * `no-sideways-entities` forbids one entity slice importing another. That
 * leaves `shared/`, which every layer may import, and which this rail can
 * live in honestly: its dependencies are `shared/api/generated/*` hooks,
 * `shared/i18n`, `shared/ui`, and MUI — nothing above `shared`.
 *
 * **Disclosed differences from the baseline:**
 *  - **No `SearchBar` of its own.** `RightPanel.jsx:51-72` owns the search
 *    string, the tag list, and a redux `resetQuery()` dispatch, and hides
 *    the bar on `/create` paths (`hooks/useSearchBar.jsx:18-27`). This app
 *    has no redux `search` slice, and each list page already owns its own
 *    query state and its own `SimpleSearchBar`. `search` is therefore a
 *    ReactNode SLOT: a page that wants the production layout moves its
 *    existing search bar into it (and keeps ownership of the state and of
 *    the `/create` decision, which it already has via its own route).
 *  - **`navRailCollapsed` is a prop, not a store read.** The baseline reads
 *    `state.settings.sideBarCollapsed`; this app keeps that in
 *    `widgets/sidebar`, which `shared/` may not import (R-L1). Callers in
 *    `pages/` can read it and pass it; the default (`false`) picks the
 *    conservative 800px threshold, i.e. the rail hides EARLIER, never later,
 *    than the baseline would.
 */
export const RAIL_WIDTH_PX = 328;

/** `PAGE_PADDING` (`common/constants.js:505`) — the rail's distance from the viewport's right edge. */
const RAIL_RIGHT_PX = 12;

/** `CARD_LIST_WIDTH` (`common/constants.js:511`) — what a page's card grid shrinks to while the rail is visible. */
export const RAIL_CONTENT_WIDTH = `calc(100% - ${String(RAIL_WIDTH_PX)}px)`;

const COLLAPSE_THRESHOLD_PX = 700;
const COLLAPSE_THRESHOLD_SIDEBAR_OPEN_PX = 800;

/** Pure half of {@link useEntityRailVisible} — `useShouldCollapseRightToolbar.js:16-19`, negated. */
export function isEntityRailVisible(windowWidth: number, navRailCollapsed: boolean): boolean {
  const threshold = navRailCollapsed ? COLLAPSE_THRESHOLD_PX : COLLAPSE_THRESHOLD_SIDEBAR_OPEN_PX;
  return windowWidth >= threshold;
}

/**
 * `true` while the rail should render. Exported because a page needs the
 * SAME answer to decide its own content width (`RAIL_CONTENT_WIDTH` vs
 * `100%`) — the baseline threads `shouldCollapseRightToolbar` through
 * `CardList.jsx` for exactly that.
 */
export function useEntityRailVisible(navRailCollapsed = false): boolean {
  const [windowWidth, setWindowWidth] = useState(() => (typeof window === 'undefined' ? COLLAPSE_THRESHOLD_SIDEBAR_OPEN_PX : window.innerWidth));

  useEffect(() => {
    const onResize = (): void => {
      setWindowWidth(window.innerWidth);
    };
    onResize();
    window.addEventListener('resize', onResize);
    return () => {
      window.removeEventListener('resize', onResize);
    };
  }, []);

  return isEntityRailVisible(windowWidth, navRailCollapsed);
}

export interface EntityRailProps {
  /** The search-bar slot — see the module doc for why the rail does not own one. */
  readonly search?: ReactNode;
  /** Tags panel, author card / trending authors — rendered in order, `gap: 16px` apart. */
  readonly children?: ReactNode;
  readonly navRailCollapsed?: boolean;
  readonly sx?: SxProps<Theme>;
  readonly 'data-testid'?: string;
}

export function EntityRail({ search, children, navRailCollapsed = false, sx, 'data-testid': dataTestId = 'entity-rail' }: EntityRailProps): ReactNode {
  const visible = useEntityRailVisible(navRailCollapsed);
  if (!visible) return null;

  return (
    <Box
      component="aside"
      data-testid={dataTestId}
      sx={combineSx(railSx, sx)}
    >
      {search}
      {children}
    </Box>
  );
}

const railSx = (theme: Theme) => ({
  position: 'fixed',
  right: `${String(RAIL_RIGHT_PX)}px`,
  top: theme.spacing(2),
  width: `${String(RAIL_WIDTH_PX)}px`,
  // CONTENT-box, deliberately. `RightPanel.jsx`'s `FixedGrid` is
  // `width: 328px; padding-left: 16px` and measures 344px wide in production,
  // i.e. the gutter sits OUTSIDE the 328px column. Under the app's global
  // `border-box` the same declarations shrank the column to 312px, so every
  // chip row, the "TAGS" heading and the author card were inset 16px further
  // from the viewport edge than production's.
  boxSizing: 'content-box',
  // A HEIGHT, not a max-height. `RightInfoPanel.jsx` sizes its column to
  // `100dvh` so the tags panel (`flex: 1`) can absorb the slack and the
  // author card below it lands on the viewport's bottom edge — which is
  // where production shows it. A max-height let the column shrink-wrap its
  // children, stacking the card immediately under the chips.
  height: `calc(100dvh - ${theme.spacing(4)})`,
  paddingLeft: theme.spacing(2),
  zIndex: 1000,
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(2),
  overflowY: 'auto',
  scrollbarWidth: 'none',
  '&::-webkit-scrollbar': { display: 'none' },
});
