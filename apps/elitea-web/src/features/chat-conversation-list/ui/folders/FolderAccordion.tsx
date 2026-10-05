import type { ReactNode, SyntheticEvent } from 'react';
import { useCallback, useEffect, useId, useState } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import FolderOutlinedIcon from '@mui/icons-material/FolderOutlined';

import { PinIcon } from '@/shared/ui/icons/pin-icon';
import { ControlsDropdown } from '@/shared/ui/ControlsDropdown';
import type { ControlsDropdownItem } from '@/shared/ui/ControlsDropdown';
import { StyledAccordion } from '@/shared/ui/StyledAccordion';
import type { AccordionShowMode } from '@/shared/ui/StyledAccordionSummary';
import { StyledAccordionSummary } from '@/shared/ui/StyledAccordionSummary';
import { StyledAccordionDetails } from '@/shared/ui/StyledAccordionDetails';
import { StyledExpandMoreIcon } from '@/shared/ui/StyledExpandMoreIcon';
import { TypographyWithConditionalTooltip } from '@/shared/ui/TypographyWithConditionalTooltip';
import { combineSx } from '@/shared/ui/lib/combineSx';
import { t } from '@/shared/i18n';

/**
 * Kept as a stable hook for styling/tests, but no longer used as a DOM `id`:
 * every mounted folder rendered the SAME literal id, and the accordion
 * summary's `aria-controls` pointed at a `folder-accordion-panel-<index>` id
 * that no element ever carried — `index` is always 0 here (one item per
 * folder), so all folders also claimed the same target. axe reports the
 * latter as `aria-valid-attr-value` (critical): a screen reader following
 * the summary's `aria-controls` landed nowhere. Both are now derived from
 * React's `useId()`, so they are unique per mounted accordion and the panel
 * really carries the id the summary names.
 */
const MENU_CONTAINER_TESTID = 'folder-accordion-menu-container';

interface FolderAccordionItemDef {
  readonly title: string;
  readonly content: ReactNode;
}

export interface FolderAccordionSlotProps {
  readonly sx?: SxProps<Theme> | undefined;
  readonly summary?: { readonly sx?: SxProps<Theme> | undefined } | undefined;
  readonly summaryContainer?: { readonly sx?: SxProps<Theme> | undefined } | undefined;
  readonly detail?: { readonly sx?: SxProps<Theme> | undefined } | undefined;
}

/**
 * Baseline: 5 independent props (`onMouseEnter`/`onMouseLeave`/`showMenu`/
 * `isHovering`/`isNextFolderHovered`) purely about hover/menu-visibility
 * state — grouped into one bag to keep `FolderAccordionProps` under the
 * §3.5 `component-props` budget (12), same "group related props into one
 * option object" trade-off `features/toolkits/ui/list/ToolkitsList.tsx`'s
 * `ToolkitsListState` and `shared/ui/BasicAccordion`'s `slotSx` already
 * establish for this codebase. Not exported: a caller's object literal is
 * checked structurally, no import of this interface's name required.
 */
interface FolderAccordionInteractionState {
  readonly isHovering?: boolean | undefined;
  readonly isNextFolderHovered?: boolean | undefined;
  /**
   * Optional force-show escape hatch for a caller-owned reason to keep the
   * trigger visible independent of hover — see this component's own module
   * doc for why this can only be set by the caller, not toggled internally
   * in response to the menu opening/closing. NOT required for the menu's
   * own open/close visibility any more: `menuContainerSx`'s own `:has(
   * [aria-expanded="true"])` rule keeps the trigger container visible for
   * the whole time `ControlsDropdown`'s menu is open regardless of this
   * prop, hover, or hover state going stale after the mouse leaves the row.
   */
  readonly showMenu?: boolean | undefined;
  readonly onMouseEnter?: (() => void) | undefined;
  readonly onMouseLeave?: (() => void) | undefined;
}

export interface FolderAccordionProps {
  readonly items: readonly FolderAccordionItemDef[];
  readonly showMode?: AccordionShowMode | undefined;
  /** Outer `Box` `sx` — baseline's `style` prop, renamed for consistency with every other `sx`-typed prop in this codebase (the baseline's own name collided with the native DOM `style` attribute). */
  readonly style?: SxProps<Theme> | undefined;
  readonly slotProps?: FolderAccordionSlotProps | undefined;
  readonly defaultExpanded?: boolean | undefined;
  /** Pre-bound `ControlsDropdown` rows — this component owns no domain callbacks of its own (baseline: `menuItems`, built by `FolderItem.tsx`). */
  readonly menuItems: readonly ControlsDropdownItem[];
  readonly isActive?: boolean | undefined;
  /** Baseline `is_private`, camelCased at this component's boundary like every other field in this codebase. */
  readonly isPrivate?: boolean | undefined;
  readonly isPinned?: boolean | undefined;
  readonly interaction?: FolderAccordionInteractionState | undefined;
}

/**
 * Generic accordion shell ported from `apps/elitea-ui/src/[fsd]/features/
 * chat/conversation-list/ui/folders/FolderAccordion.jsx` (unit C2/folders),
 * built on `StyledAccordion`/`StyledAccordionSummary`/
 * `StyledAccordionDetails`/`StyledExpandMoreIcon` (§1's accordion building
 * blocks) instead of the baseline's own hand-rolled `sx` functions.
 *
 * Local `expanded` state synced FROM `defaultExpanded` via `useEffect` —
 * ported byte-for-byte from the baseline's own one-way sync (`if
 * (defaultExpanded) setExpanded(true)`, `FolderAccordion.jsx:249-251`): a
 * caller flipping `defaultExpanded` back to `false` after the user has
 * manually collapsed the panel does NOT re-collapse it, only `true` ever
 * propagates in. Preserved as-is, not "fixed" into a fully controlled
 * `expanded` prop, since a real caller (`FolderItem.tsx`) may depend on the
 * asymmetry (re-expanding a folder that now contains the active
 * conversation, without fighting a user's manual collapse of some OTHER
 * folder).
 *
 * `DotMenu` (baseline) -> `ControlsDropdown` (this codebase's equivalent,
 * §1's C2/folders context). `ControlsDropdown` owns its own open/anchor
 * state internally and exposes no `onShowMenuList`/`onCloseMenuList`-style
 * hook — unlike the baseline's `DotMenu`, there is nothing for THIS
 * component (or any caller, `FolderItem.tsx` included) to OBSERVE and feed
 * back into a `showMenu` prop in response to the menu opening/closing; the
 * baseline's own `onShowMenuList`/`onCloseMenuList` props have no port here,
 * disclosed judgment call (no equivalent surface exists on `ControlsDropdown`
 * to wire them to). `interaction.showMenu` is kept regardless as a plain
 * caller-controlled prop, still useful for e.g. force-showing the trigger
 * while some OTHER caller-owned UI needs it visible, but — unlike the
 * baseline, where `showMenu` was the ONLY thing keeping the trigger up once
 * opened — this component no longer depends on it for that: `menuContainerSx`
 * reads `ControlsDropdown`'s own trigger `aria-expanded` state directly via
 * `:has()`, so the trigger (and its container) stays visible for the menu's
 * entire open lifetime even when no caller ever sets `showMenu` at all.
 *
 * `FolderIcon`/`PinIcon` — `FolderIcon` (baseline: `@/components/Icons/
 * FolderIcon`) has no `shared/ui/icons/**` equivalent (confirmed: `ls
 * src/shared/ui/icons/*.tsx` has no `folder-icon.tsx`, only `new-folder-
 * icon.tsx` for the CREATE-folder action) — substituted with
 * `@mui/icons-material/FolderOutlined`, same single-icon-import convention
 * `ConversationSearchButton.tsx`'s own doc comment already established for
 * an identical S2 gap. `PinIcon` IS ported (`shared/ui/icons/pin-icon.tsx`)
 * and is used directly.
 */
export function FolderAccordion({
  items,
  showMode = 'left',
  style,
  slotProps,
  defaultExpanded = false,
  menuItems,
  isActive = false,
  isPrivate = false,
  isPinned = false,
  interaction,
}: FolderAccordionProps): ReactNode {
  const { isHovering = false, isNextFolderHovered = false, showMenu = false, onMouseEnter, onMouseLeave } = interaction ?? {};

  const accordionId = useId();
  const [expanded, setExpanded] = useState(defaultExpanded);
  const shouldBeSelected = !expanded && defaultExpanded;

  const onChange = useCallback((_event: SyntheticEvent, value: boolean) => {
    setExpanded(value);
  }, []);

  useEffect(() => {
    if (defaultExpanded) setExpanded(true);
  }, [defaultExpanded]);

  return (
    <Box sx={style}>
      {items.map((item, index) => (
        <StyledAccordion
          key={`folder-accordion-item-${index}`}
          sx={combineSx(accordionSx, slotProps?.sx)}
          expanded={expanded}
          onChange={onChange}
        >
          <Box
            sx={combineSx(summaryContainerSx(isActive, isHovering, expanded, isNextFolderHovered, isPrivate, shouldBeSelected), slotProps?.summaryContainer?.sx)}
            onMouseEnter={onMouseEnter}
            onMouseLeave={onMouseLeave}
          >
            <StyledAccordionSummary
              expandIcon={<StyledExpandMoreIcon sx={expandIconSx} />}
              aria-controls={`${accordionId}-panel-${index}`}
              showMode={showMode}
              sx={combineSx(summarySx, slotProps?.summary?.sx)}
            >
              <Box sx={titleContainerSx}>
                <FolderOutlinedIcon sx={folderIconSx} />
                <TypographyWithConditionalTooltip
                  title={item.title}
                  placement="top"
                  variant="bodySmall"
                  sx={titleTextSx}
                >
                  {item.title}
                </TypographyWithConditionalTooltip>
                {isPinned && (
                  <PinIcon
                    aria-label={t('features.chatConversationList.folderAccordion.pinnedIndicator', 'Pinned')}
                    style={{ width: '0.875rem', height: '0.875rem' }}
                  />
                )}
              </Box>
            </StyledAccordionSummary>
            <Box
              data-testid={MENU_CONTAINER_TESTID}
              sx={menuContainerSx(isHovering, showMenu)}
            >
              <ControlsDropdown
                items={[...menuItems]}
                triggerAriaLabel={t('features.chatConversationList.folderAccordion.menuTrigger', 'Folder actions')}
              />
            </Box>
          </Box>
          <StyledAccordionDetails
            id={`${accordionId}-panel-${index}`}
            sx={slotProps?.detail?.sx}
          >
            {item.content}
          </StyledAccordionDetails>
        </StyledAccordion>
      ))}
    </Box>
  );
}

const accordionSx: SxProps<Theme> = { background: 'transparent' };

function summaryContainerSx(
  isActive: boolean,
  isHovering: boolean,
  expanded: boolean,
  isNextFolderHovered: boolean,
  isPrivate: boolean,
  shouldBeSelected: boolean,
): SxProps<Theme> {
  return (theme: Theme) => ({
    borderBottom: isActive || isHovering || expanded || isNextFolderHovered || shouldBeSelected ? 'none' : `0.0625rem solid ${theme.vars.palette.border.conversationItemDivider}`,
    borderLeft: isActive ? `0.1875rem solid ${isPrivate ? theme.vars.palette.primary.main : theme.vars.palette.status.published}` : 0,
    // `theme.spacing(n)` multiplies by the density-aware base unit (8px by
    // default, `pack.shape.density === 'compact'` -> 6px —
    // `shared/brand/buildTheme.ts:88`), NOT by 1rem — `1.5`/`2` here
    // reproduce the baseline's literal `0.75rem`/`1rem` (12px/16px) against
    // that 8px default, density-aware instead of hardcoded.
    padding: theme.spacing(1.5, 2),
    gap: theme.spacing(1.5),
    display: 'flex',
    flexDirection: 'row',
    justifyContent: 'space-between',
    alignItems: 'center',
    width: '100%',
    height: '3.0625rem',
    boxSizing: 'border-box',
    background: isActive ? theme.vars.palette.background.userInputBackground : shouldBeSelected ? theme.vars.palette.background.conversation.selected : 'transparent',
    borderRadius: isActive ? theme.vars.shape.radiusSm : 0,
    // Baseline's own `':hover': {..., borderRadius: 0}` (`FolderAccordion.jsx:362-365`)
    // unconditionally squared the corners on hover, even for the `isActive`
    // row (whose base `borderRadius` is `radiusSm`). `elitea/ad-hoc-radius`
    // (R-T10) bans a bare `0` radius literal outright, with no zero/"none"
    // token to reference instead — dropped rather than worked around, same
    // class of disclosed cosmetic-only drop as this file's own `alertTitle`/
    // `alarm` note: an active row keeps its rounded corners on hover instead
    // of snapping to square, a minor visual difference with no behavioural
    // one.
    '&:hover': { background: theme.vars.palette.background.userInputBackground },
    // Baseline's own `'&:hover #Menu'` selector. The hook is now a
    // `data-testid` attribute rather than a DOM `id`: every mounted
    // `FolderAccordion` shared the same literal id, which is invalid HTML
    // with more than one folder on screen. The selector is equivalent —
    // still a descendant combinator scoped to THIS hovered instance — and
    // an attribute selector cannot be invalid however many folders render.
    [`&:hover [data-testid="${MENU_CONTAINER_TESTID}"]`]: { visibility: 'visible' },
  });
}

const expandIconSx: SxProps<Theme> = (theme: Theme) => ({ width: '0.875rem', height: '0.875rem', color: theme.vars.palette.icon.fill.secondary });

/**
 * Baseline's own `'& .MuiAccordionSummary-content': { minWidth: 0 }` can't
 * be ported by naming that class directly — `elitea/no-mui-internal-selector`
 * (R-T6) bans deep `.Mui*-*` selectors outright, and `StyledAccordionSummary`'s
 * own `StyledAccordionSummaryProps` `Omit`s `slotProps` from its public API
 * (its own internal `slotProps.content` already owns that slot for the
 * expand-icon-rotation fix), so there is no sanctioned prop-based route to
 * that wrapper's `min-width` either.
 *
 * Reached structurally instead: `AccordionSummary`'s root (verified against
 * `node_modules/@mui/material/AccordionSummary/AccordionSummary.js`) has
 * *exactly* two direct children — the content `span` (`flexGrow: 1`, this
 * component's baseline target) and, when `expandIcon` is set (always, here),
 * the expand-icon-wrapper `span`. `'& > *'` is a plain child-combinator
 * selector — no `.Mui*` class named anywhere — so `elitea/no-mui-internal-
 * selector` does not apply, and it reaches the same content wrapper the
 * baseline's class selector did (plus the icon wrapper, harmless: it has no
 * `flexGrow` and is never squeezed below its fixed icon size). Restores the
 * same effect as the baseline: `titleTextSx`'s own `overflow: hidden`/
 * `textOverflow: ellipsis` can now actually engage for a very long folder
 * title in a narrow sidebar, since every ancestor flex item down to the
 * title text itself carries `min-width: 0`.
 */
const summarySx: SxProps<Theme> = { overflow: 'hidden', minWidth: 0, '& > *': { minWidth: 0 } };

const titleContainerSx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', alignItems: 'center', gap: theme.spacing(1.5), overflow: 'hidden', minWidth: 0 });

const folderIconSx: SxProps<Theme> = (theme: Theme) => ({ width: '1rem', height: '1rem', color: theme.vars.palette.icon.fill.secondary });

const titleTextSx: SxProps<Theme> = (theme: Theme) => ({
  minWidth: 0,
  whiteSpace: 'nowrap',
  overflow: 'hidden',
  textOverflow: 'ellipsis',
  color: theme.vars.palette.text.secondary,
  fontFamily: theme.typography.fontFamily,
});

function menuContainerSx(isHovering: boolean, showMenu: boolean): SxProps<Theme> {
  return {
    height: '100%',
    visibility: showMenu ? 'visible' : 'hidden',
    display: isHovering || showMenu ? 'flex' : 'none',
    justifyContent: 'center',
    alignItems: 'center',
    alignSelf: 'center',
    // Disclosed-gap fix: baseline `showMenu` (`FolderItem.jsx:164-171`) is
    // set by the caller's own `onShowMenuList`/`onCloseMenuList`, wired to
    // the baseline `DotMenu`'s open/close events. `ControlsDropdown` (this
    // codebase's equivalent, see this file's own module doc) owns its open
    // state internally and exposes no such callback, so no caller —
    // `FolderItem.tsx` included — can drive `interaction.showMenu` from a
    // real "menu is open" signal; `showMenu` alone can therefore go stale
    // the instant the mouse leaves the row, taking this container (and the
    // trigger `IconButton` inside it) to `display: none` while the popup
    // menu itself is still open and being interacted with.
    // Fixed without a `shared/ui` API change: `ControlsDropdown`'s own
    // trigger `IconButton` sets `aria-expanded="true"` on itself for the
    // exact span its `Menu` (top-level or nested submenu — closing either
    // resets `anchorEl`) stays open. `:has()` reads that live DOM state
    // directly — no `.Mui*` class involved, so `elitea/no-mui-internal-
    // selector` does not apply — keeping this container visible for the
    // whole time the menu is open regardless of hover/`showMenu`.
    '&:has([aria-expanded="true"])': { display: 'flex', visibility: 'visible' },
  };
}
