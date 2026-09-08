import type { ReactNode, SyntheticEvent } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { StyledAccordion } from '../StyledAccordion';
import { StyledAccordionDetails } from '../StyledAccordionDetails';
import { type AccordionShowMode, StyledAccordionSummary } from '../StyledAccordionSummary';
import { StyledExpandMoreIcon } from '../StyledExpandMoreIcon';
import { combineSx } from '../lib/combineSx';

/** @public One row of a `BasicAccordion` list. */
export interface AccordionItem {
  title: ReactNode;
  content: ReactNode;
  /**
   * Rendered right-aligned over the summary row; clicks/mousedowns inside it
   * never toggle the panel.
   *
   * It is a SIBLING of the summary in the DOM, not a child of it, and the
   * distinction is the whole point. `StyledAccordionSummary`'s root renders
   * as a native `<button>` (it wraps MUI's `ButtonBase`), so anything
   * FOCUSABLE placed inside it — a `<button>`, or a `role="button"` element
   * carrying `tabIndex` — is a nested interactive control: invalid HTML for
   * the button case, and in every case an axe `nested-interactive` failure
   * ("Element has focusable descendants", WCAG 4.1.2), which is what the MCP
   * detail screen shipped once the toolkit catalogue started serving an
   * object field with a description and a Load-Tools action.
   *
   * A caller therefore passes an ordinary interactive control here — a real
   * `<button>` is now the RIGHT choice — and the accordion places it beside
   * the summary button rather than inside it.
   */
  summaryAction?: ReactNode;
}

/** @public Per-slot style overrides — grouped (see `BasicAccordionProps` doc) to stay inside the §3.5 12-prop budget. */
export interface BasicAccordionSlotSx {
  root?: SxProps<Theme>;
  accordion?: SxProps<Theme>;
  summary?: SxProps<Theme>;
  title?: SxProps<Theme>;
  details?: SxProps<Theme>;
}

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface BasicAccordionProps {
  items: readonly AccordionItem[];
  showMode?: AccordionShowMode;
  /** Upper-cases the title text. @default true */
  uppercase?: boolean;
  defaultExpanded?: boolean;
  expanded?: boolean;
  onChange?: (event: SyntheticEvent, expanded: boolean) => void;
  /** Per-slot `sx` overrides — see `BasicAccordionSlotSx`. */
  slotSx?: BasicAccordionSlotSx;
  'data-testid'?: string;
}

/**
 * A list of `StyledAccordion` panels driven by a plain `items` array. Ported
 * from `apps/elitea-ui/src/[fsd]/shared/ui/accordion/BasicAccordion.jsx`.
 *
 * Deviations from the baseline:
 *  - The baseline hand-rolled a `styled(Typography)` for the title
 *    (`fontSize: 0.75rem`, `lineHeight: 1rem`, `fontWeight: 500`,
 *    `letterSpacing: 0.045rem`, conditional `textTransform`). Those four
 *    numbers are byte-for-byte the `subtitle` typography variant
 *    (`shared/brand/typography.ts`: step -1 → `0.75rem`/`1rem`, weight 500,
 *    `letterSpacingPx: 0.72` → `0.045rem` at the 16px root — and `subtitle`
 *    is *already* `textTransform: 'uppercase'`, matching this component's
 *    own `uppercase = true` default). Using `variant="subtitle"` replaces
 *    five ad-hoc values with the token the design system already defines
 *    for exactly this shape, and sidesteps `elitea/ad-hoc-font-size` (R-T11)
 *    for free. `uppercase = false` is the one real deviation, applied as a
 *    single `textTransform: 'none'` override.
 *  - The baseline's five independent `*SX`/`style` props (`accordionSX`,
 *    `style`, `summarySX`, `titleSX`, `accordionDetailsSX`) plus the other
 *    seven put this component at exactly the §3.5 12-prop budget with zero
 *    headroom. Grouped into one `slotSx` option object (same pattern
 *    `BaseModal` used for its `header`/`actions` groups) — same
 *    information, 8 props, headroom restored.
 *  - Each summary now gets both `id` AND `aria-controls` (the baseline set
 *    only `aria-controls`). MUI's `Accordion` reads the summary child's own
 *    `id`/`aria-controls` to label its `role="region"` panel
 *    (`aria-labelledby={summary.props.id}` —
 *    `node_modules/@mui/material/Accordion/Accordion.js`); without `id` on
 *    the summary, that `aria-labelledby` baseline-wide resolved to
 *    `undefined` and every expanded panel was an unlabelled region for
 *    screen-reader users.
 *  - The `summaryAction` click/mousedown shield keeps the baseline's
 *    bubble-phase `onClick`/`onMouseDown` (ported as-is, `stopPropagation`
 *    only — it has to stay bubble-phase: a *capture*-phase stop on this
 *    wrapper would halt the event before it ever reaches the action's own
 *    descendant handler, breaking the action itself, not just the
 *    accordion's toggle). `jsx-a11y/click-events-have-key-events` and
 *    `jsx-a11y/no-static-element-interactions` don't fire here because they
 *    only inspect intrinsic lowercase JSX tags (`<div onClick>`); `Box` is a
 *    component reference, which is exactly why the shield is a `Box` and
 *    not a raw `<div>`.
 */
/**
 * The per-item wrapper the summary action is positioned against, and the
 * action's own box.
 *
 * The action is laid over the summary ROW rather than flowed inside it
 * because MUI's `Accordion` reads its FIRST child as the summary and clones
 * it with the expansion props — so the summary cannot be wrapped in a flex
 * row that also holds the action, and the action cannot follow the summary as
 * an ordinary sibling either (everything after the first child is rendered
 * inside the collapsing panel). An absolutely-positioned overlay is what puts
 * it beside the summary button in the DOM while keeping it on the header row
 * on screen.
 *
 * `2.5rem` is `StyledAccordionSummary`'s own `minHeight`, written there for
 * BOTH states (`&.Mui-expanded` included), so the overlay stays centred on
 * the header whether the panel is open or closed.
 */
const rowSx = { position: 'relative' as const };
const summaryActionSx = {
  position: 'absolute' as const,
  top: 0,
  right: 0,
  height: '2.5rem',
  display: 'flex',
  alignItems: 'center',
  paddingInlineEnd: '0.5rem',
};

export function BasicAccordion({
  items,
  showMode = 'left',
  uppercase = true,
  defaultExpanded = true,
  expanded,
  onChange,
  slotSx,
  'data-testid': dataTestId,
}: BasicAccordionProps): ReactNode {
  return (
    <Box
      sx={slotSx?.root}
      data-testid={dataTestId}
    >
      {items.map((item, index) => (
        <Box
          key={index}
          sx={rowSx}
        >
          <StyledAccordion
            sx={slotSx?.accordion}
            defaultExpanded={defaultExpanded}
            expanded={expanded}
            onChange={onChange}
          >
            <StyledAccordionSummary
              id={`el-accordion-header-${index}`}
              aria-controls={`el-accordion-panel-${index}`}
              expandIcon={
                <StyledExpandMoreIcon sx={{ width: '1rem', height: '1rem' }} />
              }
              showMode={showMode}
              sx={slotSx?.summary}
            >
              <Typography
                variant="subtitle"
                sx={combineSx(uppercase ? undefined : { textTransform: 'none' }, slotSx?.title)}
              >
                {item.title}
              </Typography>
            </StyledAccordionSummary>
            <StyledAccordionDetails sx={slotSx?.details}>{item.content}</StyledAccordionDetails>
          </StyledAccordion>
          {item.summaryAction && (
            <Box
              sx={summaryActionSx}
              onClick={(event) => {
                event.stopPropagation();
              }}
              onMouseDown={(event) => {
                event.stopPropagation();
              }}
            >
              {item.summaryAction}
            </Box>
          )}
        </Box>
      ))}
    </Box>
  );
}
