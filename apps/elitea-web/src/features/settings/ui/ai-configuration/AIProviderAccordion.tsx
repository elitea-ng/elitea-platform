/**
 * AIProviderAccordion — one collapsible card on Settings › AI Providers.
 *
 * Baseline: `EliteaUI/src/[fsd]/features/settings/ui/ai-providers/
 * AIProviderAccordion.jsx`.
 *
 * This is the shape the whole page is built from and it had no counterpart
 * here: the sections were flat headings with every card of every section
 * always on screen at once, so "AI Credentials" sat below six other lists and
 * the page had no summary line anywhere. Each section is now a card carrying
 *
 *  - its title,
 *  - a count badge on the right,
 *  - and, WHILE COLLAPSED, a meta row of `label: value` pairs (the section's
 *    default model), so a closed section still answers the one question a
 *    reader has about it.
 *
 * Measured against the production page: summary padding 12px 16px, radius
 * 12px, a 1px hairline drawn as a gradient border, and details padding
 * 16px 8px 16px 28px with a 16px gap.
 */
import type { ReactNode } from 'react';
import { memo, useCallback, useState } from 'react';

import Box from '@mui/material/Box';
import SvgIcon from '@mui/material/SvgIcon';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { StyledAccordion } from '@/shared/ui/StyledAccordion';
import { StyledAccordionDetails } from '@/shared/ui/StyledAccordionDetails';
import { StyledAccordionSummary } from '@/shared/ui/StyledAccordionSummary';
import { ArrowRightIcon } from '@/shared/ui/icons/arrow-right-icon';

/** One `label: value` pair shown on the collapsed summary. */
export interface AIProviderAccordionMetaItem {
  label: ReactNode;
  value: string;
}

export interface AIProviderAccordionProps {
  title: string;
  /** Rendered in the badge on the right of the summary. */
  count?: number;
  /** Shown only while COLLAPSED — the open state shows the real controls. */
  metaItems?: AIProviderAccordionMetaItem[];
  defaultExpanded?: boolean;
  children: ReactNode;
  'data-testid'?: string;
}

export const AIProviderAccordion = memo(function AIProviderAccordion({
  title,
  count,
  metaItems = [],
  defaultExpanded = false,
  children,
  'data-testid': testId,
}: AIProviderAccordionProps) {
  const [expanded, setExpanded] = useState(defaultExpanded);

  const handleChange = useCallback((_event: React.SyntheticEvent, value: boolean) => {
    setExpanded(value);
  }, []);

  return (
    <StyledAccordion
      expanded={expanded}
      onChange={handleChange}
      sx={accordionSx}
      slotProps={{ transition: { unmountOnExit: true } }}
    >
      <StyledAccordionSummary
        data-testid={testId}
        showMode="left"
        expandIcon={
          <SvgIcon
            component={ArrowRightIcon}
            inheritViewBox
            sx={expandIconSx}
          />
        }
        sx={summarySx}
      >
        <Box sx={summaryContentSx}>
          <Box sx={summaryBodySx}>
            <Typography
              variant="headingSmall"
              sx={titleSx}
            >
              {title}
            </Typography>
            {metaItems.length > 0 && !expanded && (
              <Box sx={metaRowSx}>
                {metaItems.map((item, index) => (
                  <Box
                    key={index}
                    sx={metaItemSx}
                  >
                    {item.label}
                    <Typography
                      variant="bodyMedium"
                      sx={metaValueSx}
                    >
                      {item.value}
                    </Typography>
                  </Box>
                ))}
              </Box>
            )}
          </Box>
          {count !== undefined && (
            <Box sx={countBadgeSx}>
              <Typography
                variant="bodyMedium"
                sx={countTextSx}
              >
                {count}
              </Typography>
            </Box>
          )}
        </Box>
      </StyledAccordionSummary>
      <StyledAccordionDetails sx={detailsSx}>{children}</StyledAccordionDetails>
    </StyledAccordion>
  );
});

const accordionSx: SxProps<Theme> = { width: '100%' };

const summarySx: SxProps<Theme> = (theme) => ({
  width: '100%',
  // oxlint-disable-next-line elitea/ad-hoc-radius -- 0.75rem is the baseline's own literal and what the production card measures at; the radius tokens are 4/8/16px.
  borderRadius: '0.75rem',
  padding: '0.75rem 1rem',
  '&.Mui-expanded': { padding: '0.75rem 1rem' },
  alignItems: 'flex-start',
  gap: '0.5rem',
  border: '0.0625rem solid transparent',
  position: 'relative',
  backgroundColor: theme.vars.palette.background.aiProviderAccordion.default,
  // The hairline is a masked gradient, not a border colour: it fades from
  // 7% white at the top to nothing at the bottom, which a flat `border`
  // cannot express. Baseline `AIProviderAccordion.jsx`'s `&::before`.
  '&::before': {
    content: '""',
    position: 'absolute',
    inset: 0,
    // oxlint-disable-next-line elitea/ad-hoc-radius -- `inherit` copies the parent's token-derived radius; it is not an ad-hoc value.
    borderRadius: 'inherit',
    padding: '0.0625rem',
    background: theme.vars.palette.border.aiProviderAccordion,
    // oxlint-disable-next-line elitea/no-raw-color -- a MASK layer: only the alpha channel is read, so this is geometry, not a colour.
    mask: 'linear-gradient(#fff 0 0) content-box, linear-gradient(#fff 0 0)',
    maskComposite: 'exclude',
    WebkitMaskComposite: 'xor',
    pointerEvents: 'none',
  },
  '&:hover': {
    backgroundColor: theme.vars.palette.background.aiProviderAccordion.hover,
  },
});

const expandIconSx: SxProps<Theme> = {
  width: '1rem',
  height: '1rem',
  marginTop: '0.25rem',
};

const summaryContentSx: SxProps<Theme> = {
  width: '100%',
  display: 'flex',
  flexDirection: 'row',
  alignItems: 'flex-start',
  justifyContent: 'space-between',
  gap: '0.75rem',
};

const summaryBodySx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.75rem',
  flex: 1,
  minWidth: 0,
};

const titleSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.text.secondary,
  lineHeight: '1.5rem',
});

const metaRowSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'row',
  gap: '1.5rem',
  flexWrap: 'wrap',
  alignItems: 'center',
};

const metaItemSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'row',
  alignItems: 'center',
  gap: '0.5rem',
};

const metaValueSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.text.secondary,
  lineHeight: '1.5rem',
  whiteSpace: 'nowrap',
});

const countBadgeSx: SxProps<Theme> = (theme) => ({
  flexShrink: 0,
  minWidth: '1.5rem',
  height: '1.5rem',
  padding: '0 0.5rem',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  // oxlint-disable-next-line elitea/ad-hoc-radius -- baseline literal; see `summarySx`.
  borderRadius: '0.75rem',
  boxSizing: 'border-box',
  border: `0.0625rem solid ${theme.vars.palette.border.cardsOutlines}`,
});

const countTextSx: SxProps<Theme> = (theme) => ({
  color: theme.vars.palette.text.primary,
  lineHeight: '1.5rem',
  whiteSpace: 'nowrap',
});

const detailsSx: SxProps<Theme> = {
  padding: '1rem 0.5rem 1rem 1.75rem',
  gap: '1rem',
  width: '100%',
  boxSizing: 'border-box',
  display: 'flex',
  flexDirection: 'column',
};
