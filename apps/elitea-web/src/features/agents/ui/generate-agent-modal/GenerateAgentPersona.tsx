import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { readSoul, soulToDraft, type AgentSoulDraft } from '@/shared/lib/agentSoulDraft';
import { BaseBtn } from '@/shared/ui/BaseBtn';

/**
 * ADR-0050 decision 1a, issue 1644: the persona the generator proposed, on
 * the review form. It is read-only here (the agent's Persona tab edits it
 * after creation) and the reviewer can leave it out. Reading uses the Persona
 * tab's own draft model, so both show the same fields the same way. Every
 * value is model text: it renders as plain text.
 */
export interface GenerateAgentPersonaProps {
  readonly soul: Readonly<Record<string, unknown>>;
  readonly onRemove: () => void;
}

interface Row {
  readonly key: keyof AgentSoulDraft;
  readonly label: string;
}

function rows(): readonly Row[] {
  return [
    { key: 'displayName', label: t('features.agents.generateAgentModal.personaDisplayName', 'Display name') },
    { key: 'role', label: t('features.agents.generateAgentModal.personaRole', 'Role') },
    { key: 'voice', label: t('features.agents.generateAgentModal.personaVoice', 'Voice') },
    { key: 'values', label: t('features.agents.generateAgentModal.personaValues', 'Values') },
    { key: 'boundaries', label: t('features.agents.generateAgentModal.personaBoundaries', 'Boundaries') },
    { key: 'body', label: t('features.agents.generateAgentModal.personaBody', 'Description') },
  ];
}

export function GenerateAgentPersona({ soul, onRemove }: GenerateAgentPersonaProps): ReactNode {
  // The server checked it, but the shape is still untrusted input to a render.
  const checked = readSoul(soul);
  if (checked === undefined) return null;
  const draft = soulToDraft(checked);
  return (
    <Box sx={sectionSx} data-testid="agent-draft-persona">
      <Typography variant="subtitle" sx={labelSx}>
        {t('features.agents.generateAgentModal.personaLabel', 'Persona:')}
      </Typography>
      <Typography variant="bodySmall" sx={labelSx}>
        {t(
          'features.agents.generateAgentModal.personaHint',
          'The persona shapes how the agent speaks. It grants no tools or permissions. You can edit it later in the Persona tab.',
        )}
      </Typography>
      {rows()
        .filter((row) => draft[row.key] !== '')
        .map((row) => (
          <Box key={row.key} sx={rowSx}>
            <Typography variant="labelMedium" sx={labelSx}>
              {row.label}
            </Typography>
            <Typography variant="bodyMedium" sx={valueSx} data-testid={`agent-draft-persona-${row.key}`}>
              {draft[row.key]}
            </Typography>
          </Box>
        ))}
      <Box>
        <BaseBtn variant="iconLabel" size="small" onClick={onRemove} data-testid="agent-draft-persona-remove">
          {t('features.agents.generateAgentModal.personaRemove', 'Do not add a persona')}
        </BaseBtn>
      </Box>
    </Box>
  );
}

const sectionSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem', paddingTop: '0.5rem' };
const rowSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.125rem' };
const labelSx: SxProps<Theme> = { color: 'text.primary' };
const valueSx: SxProps<Theme> = { color: 'text.secondary', whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' };
