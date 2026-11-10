/**
 * ADR-0050 decision 6 (issue 1619) — the agent editor's Avatar tab: the
 * version's avatar pack, with a preview of every state.
 *
 * Each card shows its slot as the chat shows it (`AgentAvatar` with the
 * draft pack): a slot with its own media plays that media; an empty state
 * slot shows the still with the default motion pack. A card uploads a file
 * into the slot, or removes it. The server validates every file (type from
 * the bytes, size, length, no audio, a loop), so this tab adds no client
 * check of its own and shows the server's reason when it refuses.
 *
 * Generate makes still candidates from the persona (display name, role,
 * voice) or one state loop from the chosen still, through the project's
 * image generation model. A result only fills the draft; the editor's one
 * Save button writes the pack, like every other version field.
 */
import type { ChangeEvent, ReactNode } from 'react';
import { useCallback, useMemo, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { AVATAR_SLOTS, isVideoUrl, type AvatarSlotName } from '@elitea/client-core/conversation';

import { AgentAvatar } from '@/features/chat-messages';
import { aiConfigurationFeature } from '@/features/settings';
import type { AgentMediaObject } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { agentMediaErrorMessage, useUploadAgentMedia } from '../lib/agentMediaApi';
import { withAvatarSlot } from '../lib/avatarPackDraft';
import { isPublicAgentsProject } from '../lib/isPublicAgentsProject';
import type { EditApplicationVersionFieldsState } from '../lib/useEditApplicationVersionFields';
import { EditApplicationAvatarGenerate } from './EditApplicationAvatarGenerate';

export interface EditApplicationAvatarPanelProps {
  readonly projectId: string | undefined;
  readonly versionFields: Pick<EditApplicationVersionFieldsState, 'fields' | 'setAvatarPack'>;
  readonly disabled?: boolean | undefined;
}

/** What each slot's file input takes. The server decides; this only filters the picker. */
const STILL_ACCEPT = 'image/png,image/jpeg,image/webp,image/svg+xml';
const STATE_ACCEPT = 'image/png,image/jpeg,image/webp,video/mp4,video/webm';

function slotLabel(name: AvatarSlotName): string {
  switch (name) {
    case 'still':
      return t('pages.agents.editApplication.avatar.slot.still', 'Still');
    case 'idle':
      return t('pages.agents.editApplication.avatar.slot.idle', 'Idle');
    case 'thinking':
      return t('pages.agents.editApplication.avatar.slot.thinking', 'Thinking');
    case 'speaking':
      return t('pages.agents.editApplication.avatar.slot.speaking', 'Speaking');
    case 'working':
      return t('pages.agents.editApplication.avatar.slot.working', 'Working');
    case 'waiting':
      return t('pages.agents.editApplication.avatar.slot.waiting', 'Waiting');
    case 'success':
      return t('pages.agents.editApplication.avatar.slot.success', 'Success');
    case 'error':
      return t('pages.agents.editApplication.avatar.slot.error', 'Error');
    case 'waking':
      return t('pages.agents.editApplication.avatar.slot.waking', 'Waking');
  }
}

/** The project's image generation models as select options (the gateway takes the model name); none hides Generate. */
function useImageModels(projectId: string | undefined): readonly { readonly value: string; readonly label: string }[] {
  const models = aiConfigurationFeature.useModelsQuery(projectId ?? '', 'image_generation', !isPublicAgentsProject(projectId)).data;
  return useMemo(
    () => [...new Set((models?.items ?? []).map((model) => model.name))].map((name) => ({
      value: name,
      label: models?.items.find((model) => model.name === name)?.display_name || name,
    })),
    [models],
  );
}

export function EditApplicationAvatarPanel({ projectId, versionFields, disabled }: EditApplicationAvatarPanelProps): ReactNode {
  const imageModels = useImageModels(projectId);
  const pack = versionFields.fields.avatarPack;
  const setPack = versionFields.setAvatarPack;
  const upload = useUploadAgentMedia(projectId ?? '');
  const [target, setTarget] = useState<AvatarSlotName | undefined>(undefined);
  const [problem, setProblem] = useState<string | undefined>(undefined);
  const input = useRef<HTMLInputElement>(null);
  const still = pack?.slots.still;
  const locked = disabled === true || projectId === undefined || upload.isPending;

  const place = useCallback(
    (name: AvatarSlotName, media: AgentMediaObject) => {
      // A video slot takes the still as its poster: the server needs one,
      // and the still is what the state shows before the video plays. The
      // server takes a PNG, JPEG or WebP poster, never an SVG.
      const poster = isVideoUrl(media.url) ? still?.url : undefined;
      if (isVideoUrl(media.url) && poster === undefined) {
        setProblem(t('pages.agents.editApplication.avatar.needsStill', 'Add a still first. A video uses it as its poster.'));
        return;
      }
      if (poster !== undefined && poster.toLowerCase().endsWith('.svg')) {
        setProblem(
          t(
            'pages.agents.editApplication.avatar.svgPoster',
            'A video needs a PNG, JPEG or WebP still as its poster. Use one of those for the still, or an animated WebP for this state.',
          ),
        );
        return;
      }
      setProblem(undefined);
      setPack(
        withAvatarSlot(pack, name, {
          url: media.url,
          ...(poster !== undefined ? { poster_url: poster } : {}),
          ...(media.generated_by !== undefined ? { generated_by: media.generated_by } : {}),
        }),
      );
    },
    [pack, setPack, still],
  );

  const choose = (name: AvatarSlotName): void => {
    if (name !== 'still' && still === undefined) {
      setProblem(t('pages.agents.editApplication.avatar.needsStill', 'Add a still first. A video uses it as its poster.'));
      return;
    }
    setTarget(name);
    if (input.current !== null) {
      input.current.accept = name === 'still' ? STILL_ACCEPT : STATE_ACCEPT;
      input.current.click();
    }
  };

  const handleFile = (event: ChangeEvent<HTMLInputElement>): void => {
    const file = event.target.files?.[0];
    event.target.value = '';
    if (file === undefined || target === undefined) return;
    const name = target;
    upload.mutate(file, {
      onSuccess: (media) => place(name, media),
      onError: (error) =>
        setProblem(agentMediaErrorMessage(error, t('pages.agents.editApplication.avatar.uploadFailed', 'The file could not be uploaded.'))),
    });
  };

  return (
    <Box sx={panelSx} data-testid="edit-application-avatar-panel">
      <Typography variant="bodyMedium" color="text.secondary">
        {t(
          'pages.agents.editApplication.avatar.intro',
          'The avatar shows in chat. Each state can have its own image, animated WebP, or muted MP4 or WebM loop, at most 4 seconds, 512 px and 2 MiB. A state with no file shows the still with the default motion.',
        )}
      </Typography>
      <Box sx={gridSx}>
        {AVATAR_SLOTS.map((name) => {
          const slot = pack?.slots[name];
          return (
            <Box key={name} sx={cardSx} data-testid={`avatar-slot-${name}`} data-filled={slot !== undefined}>
              <AgentAvatar
                iconUrl={undefined}
                pack={pack}
                motion={name === 'still' || still === undefined ? 'still' : name}
                size="4rem"
              />
              <Typography variant="labelSmall">{slotLabel(name)}</Typography>
              <Typography variant="bodySmall" color="text.secondary" data-testid={`avatar-slot-${name}-source`}>
                {slot !== undefined
                  ? t('pages.agents.editApplication.avatar.own', 'Own file')
                  : name === 'still'
                    ? t('pages.agents.editApplication.avatar.none', 'No still')
                    : t('pages.agents.editApplication.avatar.default', 'Default motion')}
              </Typography>
              <Box sx={{ display: 'flex', gap: 0.5 }}>
                <Button size="small" variant="outlined" disabled={locked} onClick={() => choose(name)} data-testid={`avatar-slot-${name}-upload`}>
                  {t('pages.agents.editApplication.avatar.upload', 'Upload')}
                </Button>
                {slot !== undefined && (
                  <Button size="small" disabled={locked} onClick={() => setPack(withAvatarSlot(pack, name, undefined))} data-testid={`avatar-slot-${name}-remove`}>
                    {t('pages.agents.editApplication.avatar.remove', 'Remove')}
                  </Button>
                )}
              </Box>
            </Box>
          );
        })}
      </Box>
      <input ref={input} type="file" hidden onChange={handleFile} data-testid="avatar-file-input" />
      {problem !== undefined && (
        <Typography variant="bodySmall" color="error" data-testid="avatar-problem">
          {problem}
        </Typography>
      )}
      {imageModels.length > 0 && projectId !== undefined && (
        <EditApplicationAvatarGenerate
          projectId={projectId}
          soul={versionFields.fields.soul}
          stillUrl={still?.url}
          imageModels={imageModels}
          disabled={locked}
          onPlace={place}
        />
      )}
    </Box>
  );
}

const panelSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1rem', maxWidth: '48rem' };
const gridSx: SxProps<Theme> = { display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(9rem, 1fr))', gap: '1rem' };
const cardSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  gap: 0.5,
  padding: 1,
  border: `1px solid ${theme.vars.palette.divider}`,
  borderRadius: theme.vars.shape.radiusMd,
});
