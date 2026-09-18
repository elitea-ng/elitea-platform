import type { ReactNode } from 'react';
import { useEffect, useMemo } from 'react';

import CloseIcon from '@mui/icons-material/Close';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { SxProps, Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import {
  MAX_CONVERSATION_STARTERS,
  MAX_CONVERSATION_STARTER_LENGTH,
  MAX_DESCRIPTION_LENGTH,
  MAX_NAME_LENGTH,
  MAX_WELCOME_MESSAGE_LENGTH,
} from '@/shared/lib/limits';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { PlusIcon } from '@/shared/ui/icons/plus-icon';

import { validateAgentDraft } from '../../lib/helpers/agentDraftValidation.helpers';
import type { AgentDraft } from '../../lib/agentDraft';
import { ResourceSuggestions } from './ResourceSuggestions';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/agent/ui/generate-agent-modal/GenerateAgentReviewForm.jsx`.
 *
 * `validateAgentDraft` is A1b's real, landed
 * `src/features/agents/lib/helpers/agentDraftValidation.helpers.ts` — an
 * intra-slice import (spec: "GenerateAgentModal.jsx imports
 * `validateAgentDraft` (A1b)... intra-slice, fine"), not a guess: verified
 * present in this worktree by reading the file directly.
 *
 * The baseline's `ComponentsLib/Tooltip` wrapper has no port in this app;
 * `@mui/material`'s own `Tooltip` is used directly, same fallback-to-MUI
 * convention `shared/ui/BaseModal.tsx`'s own doc comment documents for
 * `CloseIcon` (`@mui/icons-material/Close`, also used here for the
 * remove-starter button — the baseline's hand-rolled
 * `components/Icons/CloseIcon` has no `shared/ui/icons/close-icon.tsx`
 * port either, same documented gap).
 */
/** One suggestion category's selection state — grouped per-category, then all five grouped into `GenerateAgentReviewFormProps.selection`, to stay under the §3.5 12-prop budget (13 flat props otherwise — this component originally had one `selectedIds`/`onToggle` pair per suggestion category). */
export interface SuggestionSelection {
  readonly selectedIds: ReadonlySet<number | string>;
  readonly onToggle: (id: number | string) => void;
}

export interface GenerateAgentDraftSelection {
  readonly toolkits: SuggestionSelection;
  readonly mcp: SuggestionSelection;
  readonly pipelines: SuggestionSelection;
  readonly agents: SuggestionSelection;
  readonly skills: SuggestionSelection;
}

export interface GenerateAgentReviewFormProps {
  readonly draft: AgentDraft;
  readonly onChange: (next: AgentDraft) => void;
  readonly onValidationChange?: (isValid: boolean) => void;
  readonly selection: GenerateAgentDraftSelection;
}

export function GenerateAgentReviewForm({ draft, onChange, onValidationChange, selection }: GenerateAgentReviewFormProps): ReactNode {
  const validationErrors = useMemo(() => validateAgentDraft(draft), [draft]);
  const isValid = useMemo(() => Object.keys(validationErrors).length === 0, [validationErrors]);

  useEffect(() => {
    onValidationChange?.(isValid);
  }, [isValid, onValidationChange]);

  const handleFieldChange = <K extends keyof AgentDraft>(field: K, value: AgentDraft[K]): void => {
    onChange({ ...draft, [field]: value });
  };

  const starters = draft.conversation_starters;

  const handleStarterChange = (index: number, value: string): void => {
    const updated = [...starters];
    updated[index] = value;
    onChange({ ...draft, conversation_starters: updated });
  };

  const handleRemoveStarter = (index: number): void => {
    onChange({ ...draft, conversation_starters: starters.filter((_, i) => i !== index) });
  };

  const handleAddStarter = (): void => {
    onChange({ ...draft, conversation_starters: [...starters, ''] });
  };

  const disableAddStarter =
    starters.length >= MAX_CONVERSATION_STARTERS || starters.some((s) => !s.trim());

  const addStarterTooltip =
    starters.length >= MAX_CONVERSATION_STARTERS
      ? t('features.agents.generateAgentModal.startersLimit', 'You have reached the limit of conversation starters')
      : '';

  return (
    <Box sx={containerSx}>
      <Box sx={fieldSx}>
        <Typography
          variant="labelMedium"
          sx={labelSx}
        >
          {t('features.agents.generateAgentModal.nameLabel', 'Name')}
        </Typography>
        <TextField
          fullWidth
          size="small"
          value={draft.name}
          onChange={(e) => handleFieldChange('name', e.target.value)}
          slotProps={{ htmlInput: { maxLength: MAX_NAME_LENGTH, 'data-testid': 'agent-draft-name-input' } }}
          helperText={validationErrors.name ?? `${draft.name.length}/${MAX_NAME_LENGTH}`}
          error={Boolean(validationErrors.name)}
        />
      </Box>

      <Box sx={fieldSx}>
        <Typography
          variant="labelMedium"
          sx={labelSx}
        >
          {t('features.agents.generateAgentModal.descriptionLabel', 'Description')}
        </Typography>
        <TextField
          fullWidth
          size="small"
          multiline
          minRows={2}
          maxRows={4}
          value={draft.description}
          onChange={(e) => handleFieldChange('description', e.target.value)}
          slotProps={{ htmlInput: { maxLength: MAX_DESCRIPTION_LENGTH, 'data-testid': 'agent-draft-description-input' } }}
          helperText={validationErrors.description ?? `${draft.description.length}/${MAX_DESCRIPTION_LENGTH}`}
          error={Boolean(validationErrors.description)}
        />
      </Box>

      <Box sx={fieldSx}>
        <Typography
          variant="labelMedium"
          sx={labelSx}
        >
          {t('features.agents.generateAgentModal.instructionsLabel', 'Instructions')}
        </Typography>
        <TextField
          fullWidth
          size="small"
          multiline
          minRows={4}
          maxRows={10}
          value={draft.instructions}
          onChange={(e) => handleFieldChange('instructions', e.target.value)}
          slotProps={{ htmlInput: { 'data-testid': 'agent-draft-instructions-input' } }}
        />
      </Box>

      <Box sx={fieldSx}>
        <Typography
          variant="labelMedium"
          sx={labelSx}
        >
          {t('features.agents.generateAgentModal.welcomeMessageLabel', 'Welcome Message')}
        </Typography>
        {/* elitea_issues: #5402 — multiline/scrollable like Description and Instructions above, not a single line */}
        <TextField
          fullWidth
          size="small"
          multiline
          minRows={2}
          maxRows={6}
          value={draft.welcome_message}
          onChange={(e) => handleFieldChange('welcome_message', e.target.value)}
          slotProps={{ htmlInput: { maxLength: MAX_WELCOME_MESSAGE_LENGTH, 'data-testid': 'agent-draft-welcome-message-input' } }}
          helperText={
            validationErrors.welcome_message ?? `${draft.welcome_message.length}/${MAX_WELCOME_MESSAGE_LENGTH}`
          }
          error={Boolean(validationErrors.welcome_message)}
        />
      </Box>

      {starters.length > 0 && (
        <Box sx={sectionSx}>
          <Typography
            variant="subtitle"
            sx={sectionLabelSx}
          >
            {t('features.agents.generateAgentModal.startersLabel', 'Conversation starters:')}
          </Typography>
          <Box sx={startersListSx}>
            {starters.map((starter, index) => (
              <Box
                // oxlint-disable-next-line react/no-array-index-key -- starters are positionally edited/removed, same as the baseline (`GenerateAgentReviewForm.jsx:161`); no stable id exists on a draft entry before create.
                key={index}
                sx={starterRowSx}
              >
                {/* elitea_issues: #5402 — multiline/scrollable, same as the other draft fields */}
                <TextField
                  fullWidth
                  size="small"
                  multiline
                  minRows={1}
                  maxRows={4}
                  value={starter}
                  onChange={(e) => handleStarterChange(index, e.target.value)}
                  slotProps={{ htmlInput: { maxLength: MAX_CONVERSATION_STARTER_LENGTH } }}
                  helperText={`${starter.length}/${MAX_CONVERSATION_STARTER_LENGTH}`}
                  error={!starter.trim()}
                />
                <IconButton
                  size="small"
                  onClick={() => handleRemoveStarter(index)}
                  aria-label={t('features.agents.generateAgentModal.removeStarter', 'Remove starter')}
                  sx={removeBtnSx}
                >
                  <CloseIcon sx={removeIconSx} />
                </IconButton>
              </Box>
            ))}
            <Box sx={addStarterRowSx}>
              <Tooltip
                placement="top-start"
                title={addStarterTooltip}
              >
                <Box sx={addStarterWrapperSx}>
                  <BaseBtn
                    variant="iconLabel"
                    size="small"
                    disabled={disableAddStarter}
                    onClick={handleAddStarter}
                    startIcon={<PlusIcon />}
                  >
                    {t('features.agents.generateAgentModal.addStarter', 'Starter')}
                  </BaseBtn>
                </Box>
              </Tooltip>
              <Typography
                variant="bodySmall"
                sx={addedCountSx}
              >
                {starters.length}/{MAX_CONVERSATION_STARTERS}{' '}
                {t('features.agents.generateAgentModal.startersAdded', 'added.')}
              </Typography>
            </Box>
          </Box>
        </Box>
      )}

      <ResourceSuggestions
        title={t('features.agents.generateAgentModal.suggestedToolkits', 'Suggested Toolkits:')}
        items={draft.suggested_toolkits}
        selectedIds={selection.toolkits.selectedIds}
        onToggle={selection.toolkits.onToggle}
        entityType="toolkit"
      />

      <ResourceSuggestions
        title={t('features.agents.generateAgentModal.suggestedMcp', 'Suggested MCP:')}
        items={draft.suggested_mcp}
        selectedIds={selection.mcp.selectedIds}
        onToggle={selection.mcp.onToggle}
        entityType="mcp"
      />

      <ResourceSuggestions
        title={t('features.agents.generateAgentModal.suggestedPipelines', 'Suggested Pipelines:')}
        items={draft.suggested_pipelines}
        selectedIds={selection.pipelines.selectedIds}
        onToggle={selection.pipelines.onToggle}
        entityType="pipeline"
      />

      <ResourceSuggestions
        title={t('features.agents.generateAgentModal.suggestedAgents', 'Suggested Agents:')}
        items={draft.suggested_agents}
        selectedIds={selection.agents.selectedIds}
        onToggle={selection.agents.onToggle}
        entityType="agent"
      />

      <ResourceSuggestions
        title={t('features.agents.generateAgentModal.suggestedSkills', 'Suggested Skills:')}
        items={draft.suggested_skills}
        selectedIds={selection.skills.selectedIds}
        onToggle={selection.skills.onToggle}
        entityType="skill"
      />
    </Box>
  );
}

const containerSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1rem' };
const fieldSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem' };
const labelSx: SxProps<Theme> = { color: 'text.primary' };
const sectionLabelSx: SxProps<Theme> = { color: 'text.primary' };
const sectionSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.75rem', paddingTop: '0.5rem' };
const startersListSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.75rem' };
const starterRowSx: SxProps<Theme> = { display: 'flex', alignItems: 'flex-start', gap: '0.625rem' };
const removeBtnSx: SxProps<Theme> = (theme: Theme) => ({
  backgroundColor: theme.vars.palette.background.userInputBackgroundActive,
  borderRadius: theme.vars.shape.radiusLg,
  padding: '0.375rem',
  marginTop: '0.25rem',
});
const removeIconSx: SxProps<Theme> = (theme: Theme) => ({ fontSize: theme.typography.headingMedium.fontSize });
const addStarterRowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.625rem' };
const addStarterWrapperSx: SxProps<Theme> = { display: 'inline-flex' };
const addedCountSx: SxProps<Theme> = { color: 'text.primary' };
