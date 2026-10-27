/**
 * The desktop composer's model choice: the chosen model's short name and a
 * chevron (`ComposerPicker`), opening the same model list `LLMModelSelector`
 * opens (`LLMModelsMenu`), in rows at the sidebar's scale. Settings for the
 * model are not here: the composer's ⋯ menu has "Model settings…"
 * (`widgets/chat-box`'s `ChatBoxComposerTools`).
 *
 * The trigger keeps `LLMModelSelector`'s `model-selector-name` id, so a test
 * opens either look the same way.
 */
import BusinessIcon from '@mui/icons-material/Business';
import CheckIcon from '@mui/icons-material/Check';
import PublicIcon from '@mui/icons-material/Public';

import { t } from '@/shared/i18n';
import { ComposerPicker } from '@/shared/ui/native-composer';
import { MenuRow, MenuRowIcon, MenuRowText } from '@/shared/ui/native-menu';
import type { LLMModel } from '@/widgets/llm-model-selector/lib/types';

export interface ComposerModelPickerProps {
  readonly models: readonly LLMModel[];
  readonly selectedModel: LLMModel | null | undefined;
  readonly onSelectModel: ((model: LLMModel) => void) | undefined;
  readonly disabled?: boolean | undefined;
}

/** The name without its provider path ("openai/gpt-5-mini" → "gpt-5-mini"): the row is narrow. */
function shortModelName(model: LLMModel | null | undefined): string {
  const name = model?.display_name || model?.name || '';
  return name.slice(name.lastIndexOf('/') + 1);
}

export function ComposerModelPicker({ models, selectedModel, onSelectModel, disabled }: ComposerModelPickerProps): React.JSX.Element {
  const value = shortModelName(selectedModel) || t('widgets.llmModelSelector.picker.none', 'No model');
  return (
    <ComposerPicker
      value={value}
      label={t('widgets.llmModelSelector.selector.modelTooltip', 'Select LLM Model')}
      testId="model-selector-name"
      disabled={disabled ?? false}
      minMenuWidth="16rem"
    >
      {(close) => (
        models.map((model) => {
          const selected = model.id === selectedModel?.id;
          return (
            <MenuRow
              key={model.id}
              role="menuitemradio"
              aria-checked={selected}
              title={typeof model.description === 'string' ? model.description : undefined}
              onClick={() => {
                onSelectModel?.(model);
                close();
              }}
            >
              <MenuRowIcon>{model.shared ? <PublicIcon /> : <BusinessIcon />}</MenuRowIcon>
              <MenuRowText>{model.display_name || model.name}</MenuRowText>
              <MenuRowIcon>{selected ? <CheckIcon /> : null}</MenuRowIcon>
            </MenuRow>
          );
        })
      )}
    </ComposerPicker>
  );
}
