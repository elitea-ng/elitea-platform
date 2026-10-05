/**
 * The `data` fields only a CHAT model has: its description, its two tiers, its
 * context window and its three capability flags.
 *
 * They live here rather than in the dialog for two reasons. They are offered for
 * one model type — the other four decode their `data` with unknown fields
 * refused, so a field they do not declare would turn a valid model into an
 * invalid binding — and the dialog is at its file-length budget.
 *
 * ## Why the capabilities are editable at all
 *
 * They decide what a caller may do with the model: `supports_vision` gates image
 * input, `supports_reasoning` gates the reasoning parameters, `openai_compatible`
 * decides which wire dialect the gateway speaks to it. Until this form showed
 * them, the only way to set one on a platform model was to write the row by
 * hand — and the edit dialog reset all three every time it saved, because it
 * rebuilt `data` from the fields it did show.
 */
import type { ReactNode } from 'react';

import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import FormControlLabel from '@mui/material/FormControlLabel';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { CharacterCounter } from '@/shared/ui/CharacterCounter';

import type { ModelForm } from './platformModelForm';

/** The longest model description, in characters (the server's limit too). */
const MODEL_DESCRIPTION_MAX_LENGTH = 40;

/** One labelled checkbox over one boolean field of the form. */
function Flag({
  testId,
  checked,
  label,
  onChange,
}: {
  readonly testId: string;
  readonly checked: boolean;
  readonly label: string;
  readonly onChange: (value: boolean) => void;
}): ReactNode {
  return (
    <FormControlLabel
      control={
        <BaseCheckbox
          data-testid={testId}
          checked={checked}
          onChange={(event) => {
            onChange(event.target.checked);
          }}
        />
      }
      label={label}
    />
  );
}

export interface PlatformModelChatFieldsProps {
  readonly form: ModelForm;
  readonly onChange: <K extends keyof ModelForm>(key: K, value: ModelForm[K]) => void;
}

export function PlatformModelChatFields({
  form,
  onChange,
}: PlatformModelChatFieldsProps): ReactNode {
  return (
    <>
      <div>
        <TextField
          label={t('pages.admin.platformModels.field.description', 'Description')}
          value={form.description}
          onChange={(event) => {
            onChange('description', event.target.value);
          }}
          size="small"
          fullWidth
          slotProps={{ htmlInput: { 'data-testid': 'platform-model-description', maxLength: MODEL_DESCRIPTION_MAX_LENGTH } }}
          helperText={t(
            'pages.admin.platformModels.field.descriptionHelp',
            'A few words on what the model is best for, shown under its name when people pick a model, for example Fast for everyday tasks.',
          )}
        />
        <CharacterCounter
          value={form.description}
          maxLength={MODEL_DESCRIPTION_MAX_LENGTH}
          data-testid="platform-model-description-counter"
        />
      </div>

      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.platformModels.field.tiersHelp',
          'A tier decides which of a project’s default model slots may select this model. A model in no tier is still addressable by name.',
        )}
      </Typography>
      <Flag
        testId="platform-model-low-tier"
        checked={form.lowTier}
        label={t('pages.admin.platformModels.field.lowTier', 'Offer as a low-tier default')}
        onChange={(value) => {
          onChange('lowTier', value);
        }}
      />
      <Flag
        testId="platform-model-high-tier"
        checked={form.highTier}
        label={t('pages.admin.platformModels.field.highTier', 'Offer as a high-tier default')}
        onChange={(value) => {
          onChange('highTier', value);
        }}
      />

      <TextField
        label={t('pages.admin.platformModels.field.contextWindow', 'Context window (tokens)')}
        value={form.contextWindow}
        onChange={(event) => {
          onChange('contextWindow', event.target.value);
        }}
        size="small"
        type="number"
        slotProps={{ htmlInput: { 'data-testid': 'platform-model-context-window', min: 1 } }}
        helperText={t(
          'pages.admin.platformModels.field.contextWindowHelp',
          'How much of a conversation this model can read at once. Leave it empty to keep what the model already has.',
        )}
      />

      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.platformModels.field.capabilitiesHelp',
          'What callers may ask of this model. A capability the model does not have fails at the provider, not here.',
        )}
      </Typography>
      <Flag
        testId="platform-model-supports-vision"
        checked={form.supportsVision}
        label={t('pages.admin.platformModels.field.supportsVision', 'Accepts images')}
        onChange={(value) => {
          onChange('supportsVision', value);
        }}
      />
      <Flag
        testId="platform-model-supports-reasoning"
        checked={form.supportsReasoning}
        label={t('pages.admin.platformModels.field.supportsReasoning', 'Supports reasoning')}
        onChange={(value) => {
          onChange('supportsReasoning', value);
        }}
      />
      <Flag
        testId="platform-model-openai-compatible"
        checked={form.openaiCompatible}
        label={t(
          'pages.admin.platformModels.field.openaiCompatible',
          'Speaks the OpenAI wire format',
        )}
        onChange={(value) => {
          onChange('openaiCompatible', value);
        }}
      />
    </>
  );
}
