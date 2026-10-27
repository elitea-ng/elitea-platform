/**
 * The desktop composer's model choice and ⋯ menu, after the "+" and before
 * the mic (`buildChatBoxInputSlots` puts them in the footer's leading group).
 * The web composer shows the same controls as buttons in the row; here the
 * less-used ones move into the ⋯ menu, and nothing goes missing:
 *
 * - the model gear → "Model settings…" (the same `LLMSettingsDialog`);
 * - the read-aloud play/stop → "Read replies aloud", a toggle with a check;
 * - the voice gear → "Voice settings…" (`VoiceControlButton` with its
 *   controls hidden: only its dialog renders, opened from here);
 * - the bin → "Clear chat history…" (the same `onClear`, its confirmation,
 *   its disabled state and its `chat-clear-history` id).
 *
 * `model.shown` is `NewChatInputFooterContent`'s own rule for the web's
 * selector (no active participant on the chat surface): with an agent or a
 * pipeline picked, its panel names the model, and neither the picker nor
 * "Model settings…" shows.
 */
import { useState, type ComponentProps } from 'react';

import CheckIcon from '@mui/icons-material/Check';
import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import RecordVoiceOverOutlinedIcon from '@mui/icons-material/RecordVoiceOverOutlined';
import TuneIcon from '@mui/icons-material/Tune';

import { VoiceControlButton } from '@/features/chat-input';
import { ComposerModelPicker, LLMSettingsDialog } from '@/widgets/llm-model-selector';
import type { LLMModel, LLMSettingsValues } from '@/widgets/llm-model-selector';
import { t } from '@/shared/i18n';
import { useVoiceFeatureFlags } from '@/shared/lib/hooks/useVoiceFeatureFlags';
import { ComposerMoreMenu } from '@/shared/ui/native-composer';
import { MenuDivider, MenuRow, MenuRowIcon, MenuRowText } from '@/shared/ui/native-menu';

export interface ChatBoxComposerToolsProps {
  readonly model: {
    readonly shown: boolean;
    readonly models: readonly LLMModel[];
    readonly selectedModel: LLMModel | null | undefined;
    readonly onSelectModel: ((model: LLMModel) => void) | undefined;
    readonly llmSettings: LLMSettingsValues | undefined;
    readonly onSetLLMSettings: ((settings: Readonly<Record<string, unknown>>) => void) | undefined;
  };
  readonly clearChat: { readonly disabled: boolean; readonly onClear: () => void };
  readonly voice: ComponentProps<typeof VoiceControlButton>;
}

type Dialog = 'model' | 'voice' | null;

/** The voice rows: read aloud (play/stop what was last read) and the voice settings; none while voice is off platform-wide. */
function VoiceRows({ voice, close, openVoice }: { readonly voice: ChatBoxComposerToolsProps['voice']; readonly close: () => void; readonly openVoice: () => void }) {
  const flags = useVoiceFeatureFlags();
  if (!flags.enabled) return null;
  return (
    <>
      <MenuRow
        role="menuitemcheckbox"
        aria-checked={voice.isPlaying}
        disabled={flags.temporarilyDisabled}
        data-testid="chat-voice-play-stop-button"
        onClick={() => {
          if (voice.isPlaying) voice.onStop();
          else voice.onPlay();
          close();
        }}
      >
        <MenuRowIcon>{voice.isPlaying ? <CheckIcon /> : null}</MenuRowIcon>
        <MenuRowText>{t('widgets.chatBox.composer.readAloud', 'Read replies aloud')}</MenuRowText>
      </MenuRow>
      {/* Disabled while speaking, as the gear was: the dialog's preview would talk over the reply. */}
      <MenuRow data-testid="chat-voice-settings-button" disabled={voice.isPlaying} onClick={() => { openVoice(); close(); }}>
        <MenuRowIcon><RecordVoiceOverOutlinedIcon /></MenuRowIcon>
        <MenuRowText>{t('widgets.chatBox.composer.voiceSettings', 'Voice settings…')}</MenuRowText>
      </MenuRow>
    </>
  );
}

export function ChatBoxComposerTools({ model, clearChat, voice }: ChatBoxComposerToolsProps): React.JSX.Element {
  const [dialog, setDialog] = useState<Dialog>(null);
  const { onSetLLMSettings } = model;
  return (
    <>
      {model.shown && <ComposerModelPicker models={model.models} selectedModel={model.selectedModel} onSelectModel={model.onSelectModel} />}
      <ComposerMoreMenu>
        {(close) => [
          model.shown && (
            <MenuRow key="model" disabled={!onSetLLMSettings} onClick={() => { setDialog('model'); close(); }}>
              <MenuRowIcon><TuneIcon /></MenuRowIcon>
              <MenuRowText>{t('widgets.chatBox.composer.modelSettings', 'Model settings…')}</MenuRowText>
            </MenuRow>
          ),
          <VoiceRows key="voice" voice={voice} close={close} openVoice={() => setDialog('voice')} />,
          <MenuDivider key="divider" />,
          <MenuRow key="clear" data-testid="chat-clear-history" disabled={clearChat.disabled} onClick={() => { clearChat.onClear(); close(); }}>
            <MenuRowIcon><DeleteOutlinedIcon /></MenuRowIcon>
            <MenuRowText>{t('widgets.chatBox.composer.clearHistory', 'Clear chat history…')}</MenuRowText>
          </MenuRow>,
        ]}
      </ComposerMoreMenu>
      {onSetLLMSettings && (
        <LLMSettingsDialog
          open={dialog === 'model'}
          onApply={(settings) => { onSetLLMSettings(settings); setDialog(null); }}
          onCancel={() => setDialog(null)}
          selectedModel={model.selectedModel ?? null}
          llmSettings={(model.llmSettings ?? {}) as Record<string, unknown>}
          showStepsLimit
        />
      )}
      <VoiceControlButton {...voice} hideControls settingsOpen={dialog === 'voice'} onSettingsOpenChange={(open) => setDialog(open ? 'voice' : null)} />
    </>
  );
}
