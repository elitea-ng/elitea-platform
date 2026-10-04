import type { ReactNode } from 'react';
import { useCallback, useEffect, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';

import type { TtsVoice } from '../api/ttsVoices';
import type { TtsModel } from '../lib/hooks/useTextToSpeech.types';
import type { VoiceConfig } from '../lib/hooks/useVoiceConfig.hooks';
import { useVoiceConfig } from '../lib/hooks/useVoiceConfig.hooks';
import { voiceProblemMessage } from '../lib/voiceProblems';

import { VoiceConfigControls } from './VoiceConfigControls';

/**
 * Ported from
 * apps/elitea-ui/src/[fsd]/features/chat/voice-config/ui/VoiceConfigDialog.jsx.
 *
 * Stages edits in `localConfig` (Apply/Cancel), same as the baseline.
 *
 * With no speech model configured the dialog SAYS so, above the controls:
 * the voice then comes from the browser, and a user who expected the
 * project's model would otherwise hear a different voice with no reason
 * given. The model preview goes over HTTPS (`api/voiceTransport.ts`) and
 * needs the selected project, which the `/llm` edge bills.
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
  /** The project a model voice preview bills; without it the preview uses the browser voice. */
  readonly projectId?: string | undefined;
  readonly isPlaying?: boolean | undefined;
}

export function VoiceConfigDialog(props: VoiceConfigDialogProps): ReactNode {
  const { config, voices, open, onApply, onCancel, ttsModel, hasModelTTS, projectId, isPlaying } = props;
  const [localConfig, setLocalConfig] = useState(config);

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
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
          {!hasModelTTS && (
            <Typography
              variant="bodySmall"
              color="text.secondary"
              data-testid="voice-no-model-notice"
            >
              {voiceProblemMessage('no-model')}
            </Typography>
          )}
          <VoiceConfigControls
            config={localConfig}
            onConfigChange={handleConfigChange}
            hasModelTTS={hasModelTTS}
            ttsModel={ttsModel}
            projectId={projectId}
            browserVoices={browserVoices}
            voices={voices}
            isPlaying={isPlaying}
          />
        </Box>
      }
    />
  );
}
