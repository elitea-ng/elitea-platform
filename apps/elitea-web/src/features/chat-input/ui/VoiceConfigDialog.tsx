import type { ReactNode } from 'react';
import { useCallback, useContext, useEffect, useState } from 'react';

import { t } from '@/shared/i18n';
import { SocketClientContext } from '@/shared/api/socket/client';
import { BaseModal } from '@/shared/ui/BaseModal';

import type { TtsVoice } from '../api/ttsVoices';
import type { TtsModel } from '../lib/hooks/useTextToSpeech.types';
import type { VoiceConfig } from '../lib/hooks/useVoiceConfig.hooks';
import { useVoiceConfig } from '../lib/hooks/useVoiceConfig.hooks';

import { VoiceConfigControls } from './VoiceConfigControls';

/**
 * Ported from
 * apps/elitea-ui/src/[fsd]/features/chat/voice-config/ui/VoiceConfigDialog.jsx.
 *
 * Stages edits in `localConfig` (Apply/Cancel), same as the baseline.
 *
 * Reads the socket via `useContext(SocketClientContext)` directly (NOT the
 * throwing `useSocketClient()`) — same posture as
 * `widgets/sidebar/ui/SidebarConnectionDot.tsx`: no `app/` file mounts a
 * `SocketClientContext.Provider` yet, and a missing socket is this
 * component's legitimate "fall back to the preview using browser TTS"
 * state, not a programmer error. See `useTextToSpeech.hooks.ts`'s own doc
 * comment for the identical rationale.
 *
 * PUBLIC SLOT for the sibling "voice-asr" cluster: `VoiceControlButton.jsx`
 * renders this dialog (`import { VoiceConfigDialog } from '@/features/
 * chat-input'`) — this export name and prop shape are the coordination
 * contract; see this unit's final report.
 */
export interface VoiceConfigDialogProps {
  readonly config: VoiceConfig;
  readonly voices: readonly (TtsVoice | SpeechSynthesisVoice)[];
  readonly open: boolean;
  readonly onApply: (config: VoiceConfig) => void;
  readonly onCancel: () => void;
  readonly ttsModel: TtsModel | null;
  readonly hasModelTTS: boolean;
  readonly isPlaying?: boolean | undefined;
}

export function VoiceConfigDialog(props: VoiceConfigDialogProps): ReactNode {
  const { config, voices, open, onApply, onCancel, ttsModel, hasModelTTS, isPlaying } = props;
  const [localConfig, setLocalConfig] = useState(config);

  const socket = useContext(SocketClientContext);
  const { browserVoices } = useVoiceConfig();

  useEffect(() => {
    setLocalConfig(config);
    // Baseline dependency list is `[config, open]` — re-syncs from the
    // committed `config` both when it changes AND every time the dialog
    // re-opens, discarding any un-applied edits from a previous open.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [config, open]);

  const handleConfigChange = useCallback((updates: Partial<VoiceConfig>) => {
    setLocalConfig((prev) => ({ ...prev, ...updates }));
  }, []);

  const handleApply = useCallback(() => onApply(localConfig), [localConfig, onApply]);

  return (
    <BaseModal
      open={open}
      title={t('features.chatInput.voiceConfigDialog.title', 'Voice settings')}
      onClose={onCancel}
      onConfirm={handleApply}
      actions={{
        confirmText: t('features.chatInput.voiceConfigDialog.apply', 'Apply'),
        cancelText: t('features.chatInput.voiceConfigDialog.cancel', 'Cancel'),
      }}
      content={
        <VoiceConfigControls
          config={localConfig}
          onConfigChange={handleConfigChange}
          hasModelTTS={hasModelTTS}
          ttsModel={ttsModel}
          socket={socket}
          browserVoices={browserVoices}
          voices={voices}
          isPlaying={isPlaying}
        />
      }
    />
  );
}
