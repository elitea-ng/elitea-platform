import type { CSSProperties } from 'react';

import type { Theme } from '@mui/material/styles';

/**
 * `conversationItemStyles` — `ConversationItem.jsx:511-624`, split out of
 * `ConversationItem.tsx` purely to keep that file under the §3.5
 * `max-lines` budget (400). Minus `showMenu` (see `ConversationItem.tsx`'s
 * own module doc: `ControlsDropdown` exposes no open/close callback to
 * drive it) and every `!important` (R-T5).
 *
 * `.375rem`/`1rem`/`.875rem` (baseline literal radii) map onto this app's
 * `radiusSm`/`radiusPill` tokens (R-T10 bans ad-hoc radii outright):
 * `conversationContentWrapper`'s active/hover corner-rounding uses
 * `radiusSm` (4px), the same token `ui/folders/FolderAccordion.tsx`'s own
 * near-identical "active row" background treatment already uses for the
 * same visual role; `checkedIconWrapper`/`cancelIconWrapper` are both
 * `1.75rem` (28px) squares meant to render as full circles (the baseline's
 * `1rem`/`.875rem` values are both already ≥ half that box, so any radius
 * that large draws a circle) — `radiusPill` is the token built for exactly
 * this "true pill/circle" case (see `buildTheme.ts`'s own `radiusPill` doc
 * comment: "MuiButton's `icon`/`maxi` variants need a true pill/circle,
 * which none of `radiusSm|Md|Lg` represents").
 */

export interface ConversationItemStylesParams {
  readonly isActive: boolean;
  readonly isHovering: boolean;
  readonly isNextItemHovered: boolean;
  readonly isConversationNameValid: boolean;
}

function backgroundColor(theme: Theme, isActive: boolean, isHovering: boolean): string {
  if (isActive) return theme.vars.palette.background.conversation.selected;
  if (isHovering) return theme.vars.palette.background.conversation.hover;
  return theme.vars.palette.background.conversation.normal;
}

export function conversationItemStyles(params: ConversationItemStylesParams) {
  const { isActive, isHovering, isNextItemHovered, isConversationNameValid } = params;

  return {
    conversationContentWrapper: (theme: Theme) => ({
      // #6687: the rule is always 1px; only its colour changes. With `none` on
      // hover, the fixed 2.5rem row gained 1px of content box, so its text
      // moved down half a pixel under the pointer.
      borderBottom: `1px solid ${isActive || isHovering || isNextItemHovered ? 'transparent' : theme.vars.palette.border.conversationItemDivider}`,
      padding: theme.spacing(0.625, 1.5),
      gap: theme.spacing(0.5),
      display: 'flex',
      flexDirection: 'row',
      alignItems: 'center',
      width: '100%',
      height: '2.5rem',
      boxSizing: 'border-box',
      background: backgroundColor(theme, isActive, isHovering),
      borderRadius: isActive || isHovering ? theme.vars.shape.radiusSm : 0,
      margin: 0,
    }),
    playbackIconWrapper: (theme: Theme) => ({ width: '1.25rem', height: '100%', display: 'flex', flexDirection: 'column', justifyContent: 'center', alignItems: 'center', gap: theme.spacing(0.5) }),
    mainBody: (theme: Theme) => ({ width: '100%', overflow: 'hidden', display: 'flex', alignItems: 'center', gap: theme.spacing(0.375) }),
    nameText: { whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpaceCollapse: 'preserve' as const },
    conversationIconWrapper: (theme: Theme) => ({ display: 'flex', flexDirection: 'row', gap: theme.spacing(0.375), alignItems: 'center', minWidth: 'fit-content' }),
    menuWrapper: { height: '100%', width: isHovering ? undefined : '0px', display: isHovering ? 'flex' : 'none', justifyContent: 'center', alignItems: 'center', alignSelf: 'center' },
    inputWrapper: (theme: Theme) => ({
      width: '100%',
      height: '3.125rem',
      borderRadius: theme.vars.shape.radiusSm,
      padding: theme.spacing(0.5, 2),
      display: 'flex',
      flexDirection: 'row',
      alignItems: 'center',
      gap: theme.spacing(0.75),
      background: theme.vars.palette.background.conversationEditor,
    }),
    checkedIconWrapper: (theme: Theme) => ({
      width: '1.75rem',
      height: '1.75rem',
      display: 'flex',
      justifyContent: 'center',
      alignItems: 'center',
      borderRadius: theme.vars.shape.radiusPill,
      cursor: isConversationNameValid ? 'pointer' : 'default',
      boxSizing: 'border-box',
      '&:hover': { background: isConversationNameValid ? theme.vars.palette.background.select.hover : undefined },
    }),
    cancelIconWrapper: (theme: Theme) => ({
      width: '1.75rem',
      height: '1.75rem',
      display: 'flex',
      justifyContent: 'center',
      alignItems: 'center',
      borderRadius: theme.vars.shape.radiusPill,
      cursor: 'pointer',
      boxSizing: 'border-box',
      paddingTop: theme.spacing(0.125),
      paddingLeft: theme.spacing(0.125),
      '&:hover': { background: theme.vars.palette.background.select.hover },
    }),
  };
}

export type ConversationItemStyles = ReturnType<typeof conversationItemStyles>;

/** Shared icon box size for every menu-row/row-indicator icon in this cluster (`ConversationItem.jsx`'s repeated `sx={{fontSize:'1rem'}}` on its ported icon components — those are raw SVG components here, sized via `style`, not `sx`; see `ConversationItem.menu.tsx`'s own doc comment). */
export const menuIconStyle: CSSProperties = { width: '1rem', height: '1rem' };
