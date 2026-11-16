import type { ReactNode } from 'react';

import ButtonBase from '@mui/material/ButtonBase';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { TypographyWithConditionalTooltip } from '@/shared/ui/TypographyWithConditionalTooltip';

import type { AgentToolAssociation } from '../lib/types';

/**
 * Ported from `apps/elitea-ui/src/pages/Applications/Components/Tools/CardBodies/BaseCardBody.jsx`.
 *
 * R-C1 fix: the baseline's "Show/Hide tools" toggle is a bare
 * `TypographyWithConditionalTooltip` with only `onClick` — no keyboard
 * affordance (exactly the class of defect §3.4 R-C1 names: "100 clickable
 * `<Box>`/`<Typography>` in the old app, 2 with any keyboard affordance").
 * The new app's `TypographyWithConditionalTooltip` also does not accept an
 * `onClick` prop at all (see its own doc comment — no `component`/event-prop
 * override support). This wraps it in a real `<button>` (`ButtonBase`,
 * matching `shared/ui/BannerMessage`'s identical fix for the same baseline
 * defect class) instead of reproducing the inaccessible click target.
 */
export interface BaseCardBodyProps {
  readonly tool: AgentToolAssociation;
  readonly onClickShowActions: () => void;
  readonly showActions: boolean;
}

export function BaseCardBody({ tool, onClickShowActions, showActions }: BaseCardBodyProps): ReactNode {
  // A saved toolkit instance always offers its tools for this agent to choose
  // from, whether or not the agent has chosen any yet: with no choice made the
  // toggle was missing, and so was any way to filter tools per agent (#2146).
  const offersTools = tool.tool_id !== undefined || !!tool.settings?.selected_tools?.length || !!tool.settings?.available_tools?.length;
  if (!offersTools) {
    return (
      <TypographyWithConditionalTooltip
        title={tool.description ?? ''}
        placement="top"
        variant="bodySmall"
        sx={descriptionSx}
      >
        {tool.description}
      </TypographyWithConditionalTooltip>
    );
  }

  const label = showActions ? t('agents.baseCardBody.hideTools', 'Hide tools') : t('agents.baseCardBody.showTools', 'Show tools');

  return (
    <ButtonBase
      data-testid="base-card-body-toggle"
      onClick={onClickShowActions}
      disableRipple
      sx={toggleButtonSx}
    >
      <TypographyWithConditionalTooltip
        title={label}
        placement="top"
        variant="bodySmall"
        sx={toggleTextSx}
      >
        {label}
      </TypographyWithConditionalTooltip>
    </ButtonBase>
  );
}

const descriptionSx: SxProps<Theme> = (theme: Theme) => ({
  width: '100%',
  color: theme.vars.palette.text.primary,
});

const toggleButtonSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'block',
  width: '100%',
  textAlign: 'left',
  borderRadius: theme.vars.shape.radiusSm,
  padding: '0.125rem 0',
});

const toggleTextSx: SxProps<Theme> = (theme: Theme) => ({
  width: '100%',
  color: theme.vars.palette.text.primary,
  '&:hover': {
    color: theme.vars.palette.text.createButton,
  },
});
