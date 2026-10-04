import type { ReactNode } from 'react';
import { useMemo } from 'react';

import { t } from '@/shared/i18n';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';

import { useModelsList } from '../api/models';
import type { TtsVoice } from '../api/ttsVoices';
import { useTtsVoices } from '../api/ttsVoices';
import { useSelectedProjectId } from '../api/useSelectedProjectId';
import { useVoiceConfig } from '../lib/hooks/useVoiceConfig.hooks';
import type { TtsModel } from '../lib/hooks/useTextToSpeech.types';

import { VoiceConfigControls } from './VoiceConfigControls';

function pickDefaultModel(items: readonly TtsModel[] | undefined): TtsModel | null {
  if (!items || items.length === 0) return null;
  return items.find((model) => model.default) ?? items[0] ?? null;
}

/**
 * Ported from
 * apps/elitea-ui/src/[fsd]/features/chat/voice-config/ui/VoicePersonalizationSection.jsx
 * — the settings-page section, and the sole `persist: true` `useVoiceConfig`
 * call site (every other consumer of this hook in this slice passes
 * `{persist: false}`, matching the baseline's own "only the settings page
 * writes the committed preference" split).
 *
 * Model speech needs the selected project, not a socket: it goes over HTTPS
 * (`api/voiceTransport.ts`), and the `/llm` edge bills that project.
 */
export function VoicePersonalizationSection(): ReactNode {
  const { config, setConfig, browserVoices } = useVoiceConfig({ persist: true });
  const projectId = useSelectedProjectId();

  const { data: ttsModelsData } = useModelsList({ projectId, section: 'tts', includeShared: true }, { enabled: !!projectId });
  const ttsModel = useMemo(() => pickDefaultModel(ttsModelsData?.items), [ttsModelsData]);
  const hasModelTTS = !!(ttsModel && projectId);

  const { data: ttsVoicesData } = useTtsVoices({ projectId: ttsModel?.project_id ?? projectId, modelName: ttsModel?.name }, { enabled: !!ttsModel });
  const displayVoices: readonly (TtsVoice | SpeechSynthesisVoice)[] = hasModelTTS ? (ttsVoicesData?.voices ?? []) : browserVoices;

  return (
    <BasicAccordion
      showMode="left"
      data-testid="voice-personalization-section"
      items={[
        {
          title: t('features.chatInput.voicePersonalizationSection.title', 'Voice Personalization'),
          content: (
            <VoiceConfigControls
              config={config}
              onConfigChange={setConfig}
              hasModelTTS={hasModelTTS}
              ttsModel={ttsModel}
              projectId={projectId}
              browserVoices={browserVoices}
              voices={displayVoices}
            />
          ),
        },
      ]}
    />
  );
}
