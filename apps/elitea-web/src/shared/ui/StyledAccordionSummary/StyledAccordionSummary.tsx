import type { ReactNode } from 'react';

import AccordionSummary from '@mui/material/AccordionSummary';
import type { AccordionSummaryProps } from '@mui/material/AccordionSummary';
import type { Theme } from '@mui/material/styles';

import { combineSx } from '../lib/combineSx';

/** @public Which side the expand/collapse chevron sits on. */
export type AccordionShowMode = 'left' | 'right';

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface StyledAccordionSummaryProps extends Omit<AccordionSummaryProps, 'slotProps'> {
  /** `'left'` (default): chevron leads, content flush left. `'right'`: standard trailing chevron. */
  showMode?: AccordionShowMode;
}

/**
 * The accordion header row: chevron + title (+ optional trailing action).
 * Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/accordion/StyledAccordionSummary.jsx`.
 *
 * Deviation from the baseline (MUI-9.2 hazard): the baseline reaches into
 * ITS OWN internal DOM with `'& .MuiAccordionSummary-content'` and
 * `'& .MuiAccordionSummary-expandIconWrapper.Mui-expanded'` (plus an
 * `!important` on the content margin to out-fight MUI's own expanded-state
 * CSS). `elitea/no-mui-internal-selector` (R-T6) bans deep `.Mui*-*`
 * selectors outside `shared/brand/mui-overrides/` — a directory that owns
 * neither `MuiAccordion` nor `MuiAccordionSummary` (OWNERSHIP.md's 30-key
 * baseline set never included the accordion family; the baseline itself
 * never theme-overrides it, it sx-hacks every call site instead) — and
 * `elitea/no-important-sx` (R-T5) bans the `!important` outright.
 *
 * `AccordionSummary` in this MUI version exposes `content` and
 * `expandIconWrapper` as real, documented `slotProps` (verified against
 * `node_modules/@mui/material/AccordionSummary/AccordionSummary.js`:
 * `slotProps[name]` may be a function that receives the component's own
 * `ownerState`, which carries `expanded`). That is used here instead:
 *  - the expand-icon rotation is a `&.Mui-expanded` rule on the slot;
 *  - the content margin and the row's min-height are set through the same
 *    self-scoped `&.Mui-expanded` form.
 *
 * SPECIFICITY IS THE WHOLE TRICK HERE, and getting it wrong is silent: MUI's
 * own expanded-state rules are two-class selectors, so anything this file
 * writes as a plain value (one generated class) loses to them and simply has
 * no effect while the section is open. Each of the three properties below is
 * therefore written behind `&.Mui-expanded` as well as bare.
 */
export function StyledAccordionSummary({
  showMode = 'left',
  sx,
  expandIcon,
  ...rest
}: StyledAccordionSummaryProps): ReactNode {
  const isLeft = showMode === 'left';

  return (
    <AccordionSummary
      sx={combineSx(
        (theme: Theme) => ({
          flexDirection: isLeft ? 'row-reverse' : 'row',
          minHeight: '2.5rem',
          /* Repeated behind `&.Mui-expanded` for the same specificity reason
           * as the content margin below: MUI ships a two-class rule on the
           * summary root that raises an expanded header to 64px. The baseline
           * reaches the same result from the PARENT accordion with a
           * three-class descendant selector into the summary's own classes,
           * which R-T6 does not allow here.
           *
           * The comment is a BLOCK comment on purpose. Theme-gate check 4
           * greps the file as text and strips block comments only, so prose
           * that has to describe an internal selector lives in this form —
           * the same convention `StyledAccordion` and `FolderAccordion` use. */
          '&.Mui-expanded': { minHeight: '2.5rem' },
          padding: isLeft ? theme.spacing(1) : `0 ${theme.spacing(1.5)}`,
        }),
        sx,
      )}
      expandIcon={expandIcon}
      slotProps={{
        /* The margin is written TWICE, the second time behind
         * `&.Mui-expanded`, for the same specificity reason spelled out on
         * `expandIconWrapper` below. MUI ships a two-class rule for this
         * slot's expanded state — specificity (0,2,0) — and a plain value in
         * this callback lands in one generated class, (0,1,0). So the
         * single-rule form here only ever applied while COLLAPSED: every
         * expanded accordion in the app took MUI's 20px block margins (a
         * 72px-tall header instead of 40px) and lost the 12px inline start
         * that lines the title up with the body below it. Measured against
         * the production UI, which renders `margin: 0 0 0 12px` in both
         * states. */
        content: {
          sx: (theme: Theme) => ({
            margin: 0,
            marginInlineStart: isLeft ? theme.spacing(1.5) : 0,
            '&.Mui-expanded': {
              margin: 0,
              marginInlineStart: isLeft ? theme.spacing(1.5) : 0,
            },
          }),
        },
        // The rotation is expressed as a `&.Mui-expanded` CLASS rule, not as a
        // value computed from `ownerState.expanded`.
        //
        // The computed form looked correct and rendered wrong. MUI ships its
        // own rule for this slot's expanded state, qualified by TWO classes
        // (the slot class plus the expanded class) — specificity (0,2,0). A
        // value returned from this callback lands in a single generated class,
        // (0,1,0), so MUI's rule won every time and expanded sections rotated
        // 180 degrees instead of 90: the chevron pointed LEFT rather than
        // down, on every accordion in the app. Matching the class selector
        // matches the specificity, and Emotion emits ours later, so ours wins.
        //
        // The base icon is `ArrowForwardIosSharpIcon` — a RIGHT chevron
        // (`StyledExpandMoreIcon`) — so 90 degrees is what turns it downward.
        expandIconWrapper: {
          sx: {
            transform: 'rotate(0deg)',
            '&.Mui-expanded': { transform: 'rotate(90deg)' },
          },
        },
      }}
      {...rest}
    />
  );
}
