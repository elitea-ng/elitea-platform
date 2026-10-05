/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/ui/voice-control-
 * button/VoiceControlButton.jsx`. Despite the directory name, this is a
 * TTS-SIDE (playback) component, not ASR/recording — it renders play/stop
 * controls for text-to-speech readback plus the button that opens the voice
 * settings dialog. Straightforward presentational port.
 *
 * **Cross-cluster integration fix (barrel-build pass, C3)**: this file
 * originally defined its own `renderVoiceConfigDialog` injected-slot +
 * local `VoiceConfigDialogSlotProps` shape, because as of THIS file's own
 * build, the sibling "voice-tts-config" cluster had not yet landed
 * `./VoiceConfigDialog.tsx` anywhere in this slice. That file has since
 * landed, and its own module doc states the coordination contract in the
 * other direction: "`VoiceControlButton.jsx` renders this dialog (`import {
 * VoiceConfigDialog } from '@/features/chat-input'`) — this export name and
 * prop shape are the coordination contract." Confirmed real mismatch (not
 * just a naming difference): `useReadAloud.hooks.ts`'s own `VoicePlayerProps`
 * — explicitly documented as "the props a caller spreads onto its voice
 * mini-player / control button" — has NO `renderVoiceConfigDialog` field at
 * all, so `<VoiceMiniPlayer {...readAloud.voicePlayerProps} />` could never
 * have type-checked against the old injected-slot contract. Resolved by
 * importing `VoiceConfigDialog` directly (same slice, a plain relative
 * import — no barrel entry needed) and dropping the slot entirely; this
 * component's own props are now type-identical to `VoicePlayerProps`
 * (`voiceConfig`/`voices`/`onVoiceConfigChange`/`ttsModel`/`hasModelTTS`/
 * `isPlaying`/`onStop`/`onPlay`), so any surface holding a `useReadAloud`
 * result can spread `voicePlayerProps` onto this component (or
 * `VoiceMiniPlayer`) with zero adapter.
 *
 * `VOICE_FEATURES_ENABLED`/`VOICE_FEATURES_TEMPORARILY_DISABLED`
 * (`common/constants.js:20-28`) are old-app env-derived flags with NO
 * `shared/config` `ConfigSchema` field yet — a real, disclosed gap
 * (`shared/lib/validation.ts`'s own doc comment already flags it: "Gap
 * noted for F3... not yet present in F3's ConfigSchema"). Extending that
 * schema is F3's/a cross-cutting decision, not this unit's to make
 * unilaterally (it would also gate the sibling TTS cluster's own UI).
 * Hardcoded here to the old app's own `isFlagEnabled(undefined, ...)`
 * fallback defaults (enabled: true, temporarily-disabled: false) until that
 * schema gap is closed — N4: reproduce the documented default, don't invent
 * a config mechanism this unit doesn't own.
 */
import type { ReactNode } from 'react';
import { useCallback, useState } from 'react';

import Box from '@mui/material/Box';
import Tooltip from '@mui/material/Tooltip';
import type { Theme } from '@mui/material/styles';

import { useVoiceFeatureFlags } from '@/shared/lib/hooks/useVoiceFeatureFlags';
import { t } from '@/shared/i18n';
import { BaseBtn, BUTTON_VARIANTS } from '@/shared/ui/BaseBtn';
import { GearIcon } from '@/shared/ui/icons/gear-icon';
import { PlayIcon } from '@/shared/ui/icons/play-icon';
import { StopRecordIcon } from '@/shared/ui/icons/stop-record-icon';

import type { TtsVoice } from '../api/ttsVoices';
import type { TtsModel } from '../lib/hooks/useTextToSpeech.types';
import type { VoiceConfig, VoiceConfigUpdate } from '../lib/hooks/useVoiceConfig.hooks';

import { VoiceConfigDialog } from './VoiceConfigDialog';


/** @public Type-identical to `useReadAloud.hooks.ts`'s `VoicePlayerProps` — see this file's module doc for the cross-cluster coordination this alignment fixes. */
export interface VoiceControlButtonProps {
  readonly isPlaying: boolean;
  readonly onPlay: () => void;
  readonly onStop: () => void;
  readonly voiceConfig: VoiceConfig;
  readonly voices: readonly (TtsVoice | SpeechSynthesisVoice)[];
  readonly onVoiceConfigChange: (updates: VoiceConfigUpdate) => void;
  readonly ttsModel: TtsModel | null;
  readonly hasModelTTS: boolean;
  /** The project a model voice preview bills (`/llm` edge, `X-Project-Id`). */
  readonly projectId?: string | undefined;
}

const containerSx = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  boxSizing: 'border-box' as const,
  flexShrink: 0,
  height: '100%',
  borderRadius: theme.vars.shape.radiusPill,
  padding: theme.spacing(1),
  gap: theme.spacing(1),
  boxShadow: `inset 0.0625rem 0 0 0 ${theme.vars.palette.border.lines}`,
  background: theme.vars.palette.background.tabPanel,
});

// `!important` dropped (R-T5: `elitea/no-important-sx` bans it outright) — an
// sx-generated class already outranks BaseBtn's base rule at equal
// specificity, same reasoning `features/chat-conversation-list/ui/
// conversations/ConversationSearchButton.tsx`'s own doc comment already
// established for the identical baseline pattern.
const buttonSx = (theme: Theme) => ({
  color: theme.vars.palette.text.primary,
  minWidth: '1.75rem',
  minHeight: '1.75rem',
  maxWidth: '1.75rem',
  maxHeight: '1.75rem',
  borderRadius: theme.vars.shape.radiusPill,
  padding: 0,
  '&:hover': { color: theme.vars.palette.text.secondary },
});

const iconStyle = { width: '1rem', height: '1rem' };

export function VoiceControlButton({
  isPlaying,
  onPlay,
  onStop,
  voiceConfig,
  voices,
  onVoiceConfigChange,
  ttsModel,
  hasModelTTS,
  projectId,
}: VoiceControlButtonProps): ReactNode {
  const [dialogOpen, setDialogOpen] = useState(false);
  const voiceFlags = useVoiceFeatureFlags();

  const handleDialogOpen = useCallback(() => setDialogOpen(true), []);
  const handleDialogClose = useCallback(() => setDialogOpen(false), []);
  const handleApply = useCallback(
    (config: VoiceConfig) => {
      onVoiceConfigChange(config);
      setDialogOpen(false);
    },
    [onVoiceConfigChange],
  );

  // Same platform switch as `widgets/chat`'s VoiceButton (A14, issue 200).
  // This component has no render site today — it and `VoiceMiniPlayer` are
  // exported from this slice's barrel and imported by nothing — so wiring it
  // changes nothing a user sees. It is wired anyway: leaving one of the two
  // voice controls reading a hardcoded `true` is how the next person to mount
  // it reintroduces the defect without noticing.
  if (!voiceFlags.enabled) return null;

  const playStopLabel = voiceFlags.temporarilyDisabled
    ? t('features.chatInput.voiceControlButton.disabledTooltip', 'Voice features temporarily disabled')
    : isPlaying
      ? t('features.chatInput.voiceControlButton.stopTooltip', 'Stop speaking')
      : t('features.chatInput.voiceControlButton.startTooltip', 'Start speaking');
  const settingsLabel = t('features.chatInput.voiceControlButton.settingsTooltip', 'Voice settings');

  return (
    <>
      <Box
        data-testid="chat-voice-play-stop-container"
        sx={containerSx}
      >
        <Tooltip
          title={playStopLabel}
          placement="top"
        >
          <Box component="span">
            <BaseBtn
              data-testid="chat-voice-play-stop-button"
              variant={BUTTON_VARIANTS.icon}
              color="tertiary"
              size="small"
              onClick={isPlaying ? onStop : onPlay}
              aria-label={playStopLabel}
              sx={buttonSx}
            >
              {isPlaying ? <StopRecordIcon style={iconStyle} /> : <PlayIcon style={iconStyle} />}
            </BaseBtn>
          </Box>
        </Tooltip>

        <Tooltip
          title={settingsLabel}
          placement="top"
        >
          <Box component="span">
            <BaseBtn
              data-testid="chat-voice-settings-button"
              variant={BUTTON_VARIANTS.tertiary}
              size="small"
              onClick={handleDialogOpen}
              aria-label={settingsLabel}
              disabled={isPlaying}
              sx={buttonSx}
            >
              <GearIcon style={iconStyle} />
            </BaseBtn>
          </Box>
        </Tooltip>
      </Box>
      {/*
       * `isPlaying={isPlaying}` — disclosed baseline-bug fix, not a port
       * bug. Baseline (`VoiceControlButton.jsx:83-92`) passes bare
       * `isPlaying` (JSX boolean shorthand for `isPlaying={true}`) to this
       * dialog — every sibling prop in that block is explicitly bound, so
       * this reads as an unintentional baseline typo, not deliberate
       * design. Since the settings-gear button that opens this dialog is
       * itself `disabled={isPlaying}` (both apps), the dialog can only ever
       * be opened while the real `isPlaying` is false — so baseline always
       * forwarded a hardcoded `true` while the dialog is open, permanently
       * hiding `VoiceConfigControls`' "Preview Voice" button (gated on
       * `!isPlaying`) even though playback is genuinely stopped. Forwarding
       * the real prop here is the more-correct behavior and surfaces
       * Preview Voice as intended; kept as the real value rather than
       * reproducing the baseline typo.
       */}
      <VoiceConfigDialog
        config={voiceConfig}
        voices={voices}
        open={dialogOpen}
        onApply={handleApply}
        onCancel={handleDialogClose}
        ttsModel={ttsModel}
        hasModelTTS={hasModelTTS}
        projectId={projectId}
        isPlaying={isPlaying}
      />
    </>
  );
}
