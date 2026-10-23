/**
 * The composer footer's model chip (issue 1228): the model the next turn
 * runs on, with its reasoning effort, and a menu of the project's chat
 * models; for a model that `supports_reasoning`, the effort too. `/model`
 * opens the same menu (it clicks the chip, like `/agent`).
 *
 * The page hides it when the agent pins its model or the workspace turns
 * the choice off. Choosing here only says what the next turn asks for: the
 * host checks it before the turn starts and the gateway governs it.
 */
import { useState, type Ref } from 'react';

import CheckOutlinedIcon from '@mui/icons-material/CheckOutlined';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Divider from '@mui/material/Divider';
import ListSubheader from '@mui/material/ListSubheader';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { REASONING_EFFORTS, type ReasoningEffort, type TurnModelChoice } from '@/shared/desktop/turnModel';
import { t } from '@/shared/i18n';

/** One chat model of the project, as the menu offers it. */
export interface ModelOption {
  name: string;
  description?: string | undefined;
  supportsReasoning: boolean;
}

export interface ModelChipProps {
  /** The model the next turn runs on; `null` while the agent's is not known yet. */
  current: string | null;
  effort?: ReasoningEffort | undefined;
  models: readonly ModelOption[];
  onChoose: (choice: TurnModelChoice) => void;
  /** The box holding the chip; `/model` clicks its button. */
  chipRef?: Ref<HTMLDivElement> | undefined;
}

const chipSx = (theme: Theme) => ({
  minHeight: 22,
  gap: 0.5,
  padding: '0 6px',
  borderRadius: '6px',
  fontSize: '0.75rem',
  fontWeight: 500,
  color: theme.vars.palette.text.secondary,
  maxWidth: 220,
  '&:hover, &[aria-expanded="true"]': { background: `color-mix(in srgb, ${theme.vars.palette.text.primary} 8%, transparent)` },
  '& svg': { fontSize: 14 },
});

const ellipsis = { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } as const;

function effortLabel(effort: ReasoningEffort): string {
  switch (effort) {
    case 'none':
      return t('workspace.model.effort.none', 'No reasoning');
    case 'low':
      return t('workspace.model.effort.low', 'Low');
    case 'medium':
      return t('workspace.model.effort.medium', 'Medium');
    case 'high':
      return t('workspace.model.effort.high', 'High');
  }
}

/** "gpt-5 · High", or "Agent's model" before the agent's is known. */
export function modelChipLabel(current: string | null, effort: ReasoningEffort | undefined): string {
  const name = current ?? t('workspace.model.agentDefault', 'Agent’s model');
  return effort === undefined ? name : `${name} · ${effortLabel(effort)}`;
}

function Check({ on }: { on: boolean }): React.JSX.Element {
  return (
    <Box component="span" sx={{ width: 18, display: 'inline-flex', flexShrink: 0 }}>
      {on && <CheckOutlinedIcon fontSize="inherit" />}
    </Box>
  );
}

export function ModelChip({ current, effort, models, onChoose, chipRef }: ModelChipProps): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const label = modelChipLabel(current, effort);
  const reasoning = models.find((model) => model.name === current)?.supportsReasoning === true;
  const choose = (choice: TurnModelChoice): void => {
    setAnchor(null);
    onChoose(choice);
  };
  return (
    <Box ref={chipRef} sx={{ display: 'inline-flex', minWidth: 0 }}>
      <ButtonBase
        data-testid="model-chip"
        aria-haspopup="menu"
        aria-expanded={anchor !== null}
        aria-label={t('workspace.model.choose', 'Model: {{label}}', { label })}
        onClick={(event) => setAnchor(event.currentTarget)}
        sx={chipSx}
      >
        <Box component="span" sx={ellipsis}>
          {label}
        </Box>
        <ExpandMoreIcon />
      </ButtonBase>
      <Menu
        anchorEl={anchor}
        open={anchor !== null}
        onClose={() => setAnchor(null)}
        anchorOrigin={{ vertical: 'top', horizontal: 'left' }}
        transformOrigin={{ vertical: 'bottom', horizontal: 'left' }}
        slotProps={{ list: { dense: true, 'aria-label': t('workspace.model', 'Model') } }}
      >
        <ListSubheader sx={{ lineHeight: '22px', background: 'transparent' }}>{t('workspace.model', 'Model')}</ListSubheader>
        {models.length === 0 && <MenuItem disabled>{t('workspace.model.none', 'No other models in this project')}</MenuItem>}
        {models.map((model) => (
          <MenuItem key={model.name} selected={model.name === current} onClick={() => choose({ name: model.name })} sx={{ alignItems: 'flex-start' }}>
            <Check on={model.name === current} />
            <Box sx={{ display: 'flex', flexDirection: 'column', minWidth: 0 }}>
              <Box component="span" sx={ellipsis}>
                {model.name}
              </Box>
              {model.description !== undefined && (
                <Typography variant="bodySmall" sx={(theme: Theme) => ({ ...ellipsis, color: theme.vars.palette.text.metrics })}>
                  {model.description}
                </Typography>
              )}
            </Box>
          </MenuItem>
        ))}
        {reasoning && current !== null && <Divider />}
        {reasoning && current !== null && <ListSubheader sx={{ lineHeight: '22px', background: 'transparent' }}>{t('workspace.model.effort', 'Reasoning effort')}</ListSubheader>}
        {reasoning &&
          current !== null &&
          REASONING_EFFORTS.map((level) => (
            <MenuItem key={level} selected={level === effort} data-testid={`model-effort-${level}`} onClick={() => choose({ name: current, reasoning_effort: level })}>
              <Check on={level === effort} />
              {effortLabel(level)}
            </MenuItem>
          ))}
      </Menu>
    </Box>
  );
}
