/**
 * ADR-0050 decision 1a — the agent editor's Persona tab: the version's soul.
 *
 * The soul shapes how the agent speaks and behaves (name, voice, values,
 * boundaries, relationships, a free-form body) and grants nothing; the
 * platform puts it into the prompt after the organization's guardrails and
 * before the instructions. It is saved with the version by the editor's one
 * Save button, like every other version field, so the unsaved-changes guard
 * covers it.
 *
 * List fields take one entry per line; relationships take one
 * `agent: stance` pair per line. Emptying every field removes the soul.
 */
import type { ChangeEvent, ReactNode } from 'react';
import { useCallback, useMemo } from 'react';

import Box from '@mui/material/Box';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { SOUL_MAX_TEXT_BYTES, soulFromDraft, soulTextBytes, type AgentSoulDraft } from '../lib/agentSoulDraft';
import type { EditApplicationVersionFieldsState } from '../lib/useEditApplicationVersionFields';

export interface EditApplicationPersonaPanelProps {
  readonly versionFields: Pick<EditApplicationVersionFieldsState, 'fields' | 'setSoul'>;
  readonly disabled?: boolean | undefined;
}

interface FieldSpec {
  readonly key: keyof AgentSoulDraft;
  readonly label: string;
  readonly helper?: string;
  readonly multiline?: boolean;
  readonly maxLength?: number;
}

function fieldSpecs(): readonly FieldSpec[] {
  return [
    {
      key: 'displayName',
      label: t('pages.agents.editApplication.persona.displayName', 'Display name'),
      helper: t(
        'pages.agents.editApplication.persona.displayNameHelper',
        'The name shown in chat. It can differ from the agent name.',
      ),
      maxLength: 64,
    },
    {
      key: 'pronouns',
      label: t('pages.agents.editApplication.persona.pronouns', 'Pronouns'),
      maxLength: 32,
    },
    {
      key: 'role',
      label: t('pages.agents.editApplication.persona.role', 'Role'),
      helper: t('pages.agents.editApplication.persona.roleHelper', 'One line, shown in the participant list.'),
      maxLength: 160,
    },
    {
      key: 'voice',
      label: t('pages.agents.editApplication.persona.voice', 'Voice'),
      helper: t('pages.agents.editApplication.persona.oneEntryPerLine', 'One entry per line.'),
      multiline: true,
    },
    {
      key: 'values',
      label: t('pages.agents.editApplication.persona.values', 'Values'),
      helper: t('pages.agents.editApplication.persona.oneEntryPerLine', 'One entry per line.'),
      multiline: true,
    },
    {
      key: 'boundaries',
      label: t('pages.agents.editApplication.persona.boundaries', 'Boundaries'),
      helper: t(
        'pages.agents.editApplication.persona.boundariesHelper',
        'What the agent declines or always escalates. One entry per line.',
      ),
      multiline: true,
    },
    {
      key: 'relationships',
      label: t('pages.agents.editApplication.persona.relationships', 'Relationships'),
      helper: t('pages.agents.editApplication.persona.relationshipsHelper', 'One "agent: stance" pair per line.'),
      multiline: true,
    },
    {
      key: 'proactiveGoals',
      label: t('pages.agents.editApplication.persona.proactiveGoals', 'Proactive goals'),
      helper: t(
        'pages.agents.editApplication.persona.proactiveGoalsHelper',
        'Read by the heartbeat, not put into the prompt. One entry per line.',
      ),
      multiline: true,
    },
    {
      key: 'body',
      label: t('pages.agents.editApplication.persona.body', 'Body'),
      helper: t(
        'pages.agents.editApplication.persona.bodyHelper',
        'Free-form markdown, the text of SOUL.md after its front matter.',
      ),
      multiline: true,
    },
  ];
}

export function EditApplicationPersonaPanel({ versionFields, disabled }: EditApplicationPersonaPanelProps): ReactNode {
  const { soul } = versionFields.fields;
  const onSoulChange = versionFields.setSoul;
  const specs = useMemo(() => fieldSpecs(), []);
  const bytes = useMemo(() => soulTextBytes(soulFromDraft(soul)), [soul]);
  const tooLarge = bytes > SOUL_MAX_TEXT_BYTES;

  const handleChange = useCallback(
    (key: keyof AgentSoulDraft) => (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) =>
      onSoulChange({ ...soul, [key]: event.target.value }),
    [soul, onSoulChange],
  );

  return (
    <Box sx={panelSx} data-testid="edit-application-persona-panel">
      <Typography variant="bodyMedium" color="text.secondary">
        {t(
          'pages.agents.editApplication.persona.intro',
          'The persona shapes how this agent speaks and behaves. It grants no tools or permissions. Leave every field empty for no persona.',
        )}
      </Typography>
      {specs.map((spec) => (
        <TextField
          key={spec.key}
          label={spec.label}
          helperText={spec.helper}
          value={soul[spec.key]}
          onChange={handleChange(spec.key)}
          disabled={disabled}
          multiline={spec.multiline === true}
          minRows={spec.key === 'body' ? 6 : spec.multiline === true ? 2 : undefined}
          slotProps={{
            htmlInput: {
              'data-testid': `persona-${spec.key}`,
              ...(spec.maxLength !== undefined ? { maxLength: spec.maxLength } : {}),
            },
          }}
          fullWidth
        />
      ))}
      <Typography variant="bodySmall" color={tooLarge ? 'error' : 'text.secondary'} data-testid="persona-size">
        {t('pages.agents.editApplication.persona.size', '{{bytes}} of {{max}} bytes', {
          bytes: String(bytes),
          max: String(SOUL_MAX_TEXT_BYTES),
        })}
      </Typography>
    </Box>
  );
}

const panelSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '1rem',
  maxWidth: '48rem',
};
