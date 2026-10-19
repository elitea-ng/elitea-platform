/**
 * Port of EliteaUI `features/chat/ui/sub-agent-section/SubAgentAccordion.jsx`.
 *
 * One collapsible group for one sub-agent instance: a pill summary with the
 * header icon and the breadcrumb label, collapsed by default, a
 * left-to-right shimmer plus a spinner while the instance runs, and no
 * shimmer while it is `paused`. Callers opt into `defaultExpanded` (the
 * approval buckets do); after mount the reader owns the state. `transparent`
 * drops the accordion's own surface so only the wrapped card carries styling.
 */
import type { ReactNode, SyntheticEvent } from 'react';
import { useCallback, useState } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import type { Theme } from '@mui/material/styles';
import { useTheme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { StyledAccordion } from '@/shared/ui/StyledAccordion';
import { StyledAccordionDetails } from '@/shared/ui/StyledAccordionDetails';
import { StyledAccordionSummary } from '@/shared/ui/StyledAccordionSummary';
import { ArrowRightIcon } from '@/shared/ui/icons/arrow-right-icon';

import type { SubAgentTool } from './subAgentIcon.helpers';
import { resolveSubAgentIcon } from './subAgentIcon.helpers';

/** @public Props for `SubAgentAccordion` — the reference's own prop set. */
export interface SubAgentAccordionProps {
  readonly name: string;
  /** Breadcrumb label (`Parent (1) ▸ Sub`); falls back to `name`. */
  readonly label?: string | undefined;
  readonly tools?: readonly SubAgentTool[] | undefined;
  readonly agentType?: string | undefined;
  readonly running?: boolean;
  readonly paused?: boolean;
  readonly defaultExpanded?: boolean;
  readonly transparent?: boolean;
  readonly children?: ReactNode;
}

const styles = {
  accordion: (theme: Theme) => ({
    width: '100%',
    paddingBottom: '0.5rem',
    borderBottom: `0.0625rem solid ${theme.vars.palette.border.lines}`,
    '&.Mui-expanded': { margin: 0 },
    '& .MuiAccordion-heading': { display: 'inline-block' },
  }),
  transparent: {
    backgroundColor: 'transparent',
    backgroundImage: 'none',
    border: 'none',
    // eslint-disable-next-line elitea/ad-hoc-radius — reference `transparent` resets the radius to 0
    borderRadius: 0,
    boxShadow: 'none',
  },
  summary: (theme: Theme) => ({
    width: 'auto',
    // eslint-disable-next-line elitea/ad-hoc-radius — reference pill summary
    borderRadius: '1rem',
    minHeight: '1.5rem',
    '&.Mui-expanded': { minHeight: '1.5rem' },
    padding: '0 0.5rem',
    '&:hover': { backgroundColor: theme.vars.palette.background.chatContinueBackground },
  }),
  summaryContent: { display: 'flex', flexDirection: 'row', alignItems: 'center', gap: '0.5rem' },
  label: (theme: Theme) => ({ color: theme.vars.palette.text.secondary, fontWeight: 600, whiteSpace: 'nowrap' }),
  labelRunning: (theme: Theme) => ({
    fontWeight: 600,
    whiteSpace: 'nowrap',
    background: `linear-gradient(90deg, ${theme.vars.palette.text.secondary} 0%, ${theme.vars.palette.text.secondary} 35%, ${theme.vars.palette.text.primary} 50%, ${theme.vars.palette.text.secondary} 65%, ${theme.vars.palette.text.secondary} 100%)`,
    backgroundSize: '200% auto',
    color: 'transparent',
    WebkitBackgroundClip: 'text',
    backgroundClip: 'text',
    WebkitTextFillColor: 'transparent',
    animation: 'subAgentNameShimmer 1.8s linear infinite',
    '@keyframes subAgentNameShimmer': {
      '0%': { backgroundPosition: '200% center' },
      '100%': { backgroundPosition: '-200% center' },
    },
  }),
  spinner: (theme: Theme) => ({ color: theme.vars.palette.text.secondary, flexShrink: 0 }),
  details: {
    paddingTop: '0.5rem',
    paddingBottom: '0.5rem',
    paddingLeft: '1.75rem',
    paddingRight: '0.5rem',
    gap: '0.75rem',
    width: '100%',
    boxSizing: 'border-box',
    display: 'flex',
    flexDirection: 'column',
  },
} as const;

export function SubAgentAccordion({
  name,
  label,
  tools,
  agentType,
  running = false,
  paused = false,
  defaultExpanded = false,
  transparent = false,
  children,
}: SubAgentAccordionProps): ReactNode {
  const theme = useTheme();
  const [expanded, setExpanded] = useState(defaultExpanded);
  const onChange = useCallback((_event: SyntheticEvent, value: boolean) => setExpanded(value), []);
  const shimmer = running && !paused;
  const icon = resolveSubAgentIcon(name, tools, theme, agentType);
  const Icon = icon?.component;

  return (
    <StyledAccordion
      expanded={expanded}
      onChange={onChange}
      sx={[styles.accordion, transparent && styles.transparent]}
      slotProps={{ transition: { unmountOnExit: true } }}
    >
      <StyledAccordionSummary
        data-testid={`chat-answer-nested-agent-accordion-summary-${name}`}
        expandIcon={<ArrowRightIcon style={{ width: '1rem', height: '1rem' }} />}
        showMode="left"
        sx={styles.summary}
      >
        <Box sx={styles.summaryContent}>
          {Icon && <Box sx={icon.sx}><Icon /></Box>}
          <Typography variant="bodySmall" sx={shimmer ? styles.labelRunning : styles.label}>
            {label || name}
          </Typography>
          {shimmer && <CircularProgress size={12} thickness={5} sx={styles.spinner} />}
        </Box>
      </StyledAccordionSummary>
      <StyledAccordionDetails data-testid={`chat-answer-nested-agent-accordion-details-${name}`} sx={styles.details}>
        {children}
      </StyledAccordionDetails>
    </StyledAccordion>
  );
}
