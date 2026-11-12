/**
 * ADR-0050 decision 6, "Generation" — the Avatar tab's Generate row.
 *
 * "Generate stills" asks the project's image generation model for four
 * candidates from the persona; choosing one puts it in the still slot.
 * "Generate loop" asks for the pose frames of one state from the still and
 * the server assembles them into a looping animated WebP, which goes in that
 * state's slot. The project budget pays for every call. There is no video
 * generation.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import ButtonBase from '@mui/material/ButtonBase';
import Typography from '@mui/material/Typography';

import type { AvatarSlotName } from '@elitea/client-core/conversation';
import { AGENT_STATE, type AgentState } from '@elitea/client-core/stream';

import type { AgentMediaObject } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { SingleSelect } from '@/shared/ui/SingleSelect';

import { agentMediaErrorMessage, useGenerateAgentMedia } from '../lib/agentMediaApi';
import type { AgentSoulDraft } from '@/shared/lib/agentSoulDraft';

export interface EditApplicationAvatarGenerateProps {
  readonly projectId: string;
  readonly soul: AgentSoulDraft;
  readonly stillUrl: string | undefined;
  readonly imageModels: readonly { readonly value: string; readonly label: string }[];
  readonly disabled: boolean;
  readonly onPlace: (slot: AvatarSlotName, media: AgentMediaObject) => void;
}

function lines(text: string): string[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '')
    .slice(0, 16);
}

/** A loop can be made for every state that moves; `idle` breathes on the still. */
const LOOP_STATES: readonly AgentState[] = Object.values(AGENT_STATE);

export function EditApplicationAvatarGenerate(props: EditApplicationAvatarGenerateProps): ReactNode {
  const { projectId, soul, stillUrl, imageModels, disabled, onPlace } = props;
  const generate = useGenerateAgentMedia(projectId);
  const [model, setModel] = useState(imageModels[0]?.value ?? '');
  const [state, setState] = useState<AgentState>(AGENT_STATE.Thinking);
  const [candidates, setCandidates] = useState<readonly AgentMediaObject[]>([]);
  const [problem, setProblem] = useState<string | undefined>(undefined);
  const busy = disabled || generate.isPending || model === '';
  const failed = (error: unknown): void =>
    setProblem(agentMediaErrorMessage(error, t('pages.agents.editApplication.avatar.generateFailed', 'The image model could not make the avatar.')));

  const stills = (): void => {
    setProblem(undefined);
    generate.mutate(
      { kind: 'still', model, display_name: soul.displayName, role: soul.role, voice: lines(soul.voice), count: 4 },
      { onSuccess: setCandidates, onError: failed },
    );
  };

  const loop = (): void => {
    if (stillUrl === undefined) return;
    setProblem(undefined);
    generate.mutate(
      { kind: 'loop', model, state, still_url: stillUrl },
      { onSuccess: (media) => media[0] !== undefined && onPlace(state, media[0]), onError: failed },
    );
  };

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }} data-testid="avatar-generate">
      <Typography variant="labelMedium">{t('pages.agents.editApplication.avatar.generate', 'Generate')}</Typography>
      <Box sx={{ display: 'flex', gap: 1, alignItems: 'flex-end', flexWrap: 'wrap' }}>
        <SingleSelect
          label={t('pages.agents.editApplication.avatar.model', 'Image model')}
          value={model}
          onChange={setModel}
          options={imageModels.map((option) => ({ value: option.value, label: option.label }))}
          disabled={busy}
        />
        <Button size="small" variant="outlined" disabled={busy} onClick={stills} data-testid="avatar-generate-stills">
          {t('pages.agents.editApplication.avatar.generateStills', 'Generate stills')}
        </Button>
        <SingleSelect
          label={t('pages.agents.editApplication.avatar.loopState', 'State')}
          value={state}
          onChange={(value) => setState(value as AgentState)}
          options={LOOP_STATES.map((value) => ({ value, label: value }))}
          disabled={busy || stillUrl === undefined}
        />
        <Button size="small" variant="outlined" disabled={busy || stillUrl === undefined} onClick={loop} data-testid="avatar-generate-loop">
          {t('pages.agents.editApplication.avatar.generateLoop', 'Generate loop')}
        </Button>
      </Box>
      {candidates.length > 0 && (
        <Box sx={{ display: 'flex', gap: 1 }} data-testid="avatar-candidates">
          {candidates.map((media) => (
            <ButtonBase
              key={media.url}
              disabled={disabled}
              onClick={() => onPlace('still', media)}
              aria-label={t('pages.agents.editApplication.avatar.useCandidate', 'Use this still')}
              data-testid="avatar-candidate"
            >
              <Box component="img" src={media.url} alt="" sx={{ width: '4rem', height: '4rem', objectFit: 'cover' }} />
            </ButtonBase>
          ))}
        </Box>
      )}
      {problem !== undefined && (
        <Typography variant="bodySmall" color="error" data-testid="avatar-generate-problem">
          {problem}
        </Typography>
      )}
    </Box>
  );
}
