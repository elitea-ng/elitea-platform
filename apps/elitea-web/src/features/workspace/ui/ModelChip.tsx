/**
 * The composer footer's model chip (issue 1228): the model the next turn
 * runs on, with its reasoning effort, drawn as a native pop-up button
 * (`ComposerPicker`, "gpt-5 · High ⌄"), and a menu of the project's chat
 * models; for a model that `supports_reasoning`, the effort too. `/model`
 * opens the same menu (it clicks the chip, like `/agent`).
 *
 * The page hides it when the agent pins its model or the workspace turns
 * the choice off. Choosing here only says what the next turn asks for: the
 * host checks it before the turn starts and the gateway governs it.
 */
import type { Ref } from 'react';

import CheckOutlinedIcon from '@mui/icons-material/CheckOutlined';
import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { REASONING_EFFORTS, type ReasoningEffort, type TurnModelChoice } from '@/shared/desktop/turnModel';
import { t } from '@/shared/i18n';
import { ComposerPicker } from '@/shared/ui/native-composer';
import { MenuDivider, MenuHeader, MenuRow, MenuRowIcon, MenuRowText } from '@/shared/ui/native-menu';

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

/** The model menu's rows: the project's chat models, then, for one that reasons, its effort. */
function modelRows({ current, effort, models }: Omit<ModelChipProps, 'onChoose' | 'chipRef'>, choose: (choice: TurnModelChoice) => void): React.JSX.Element[] {
  const reasoning = models.find((model) => model.name === current)?.supportsReasoning === true;
  const rows: React.JSX.Element[] = [<MenuHeader key="header">{t('workspace.model', 'Model')}</MenuHeader>];
  if (models.length === 0) {
    rows.push(
      <MenuRow key="none" disabled>
        <MenuRowText>{t('workspace.model.none', 'No other models in this project')}</MenuRowText>
      </MenuRow>,
    );
  }
  for (const model of models) {
    rows.push(
      <MenuRow key={model.name} selected={model.name === current} onClick={() => choose({ name: model.name })}>
        <MenuRowIcon>{model.name === current && <CheckOutlinedIcon />}</MenuRowIcon>
        <Box sx={{ display: 'flex', flexDirection: 'column', flex: 1, minWidth: 0 }}>
          <MenuRowText>{model.name}</MenuRowText>
          {model.description !== undefined && (
            <Typography variant="bodySmall" noWrap sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
              {model.description}
            </Typography>
          )}
        </Box>
      </MenuRow>,
    );
  }
  if (!reasoning || current === null) return rows;
  rows.push(<MenuDivider key="divider" />, <MenuHeader key="effort">{t('workspace.model.effort', 'Reasoning effort')}</MenuHeader>);
  for (const level of REASONING_EFFORTS) {
    rows.push(
      <MenuRow key={level} selected={level === effort} data-testid={`model-effort-${level}`} onClick={() => choose({ name: current, reasoning_effort: level })}>
        <MenuRowIcon>{level === effort && <CheckOutlinedIcon />}</MenuRowIcon>
        <MenuRowText>{effortLabel(level)}</MenuRowText>
      </MenuRow>,
    );
  }
  return rows;
}

export function ModelChip({ current, effort, models, onChoose, chipRef }: ModelChipProps): React.JSX.Element {
  return (
    <Box ref={chipRef} sx={{ display: 'inline-flex', minWidth: 0 }}>
      <ComposerPicker label={t('workspace.model', 'Model')} value={modelChipLabel(current, effort)} testId="model-chip" minMenuWidth="14rem">
        {(close) =>
          modelRows({ current, effort, models }, (choice) => {
            close();
            onChoose(choice);
          })
        }
      </ComposerPicker>
    </Box>
  );
}
