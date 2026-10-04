/** Type definitions for LLM Model Selector widget. */

export interface LLMModel {
  id: string;
  name: string;
  display_name?: string;
  /** The admin's one-line text on the model (max 40 characters). The menu shows it under the name. */
  description?: string;
  shared?: boolean;
  supports_vision?: boolean;
  supports_reasoning?: boolean;
  max_output_tokens?: number;
}

export interface LLMSettingsValues {
  temperature?: number;
  max_tokens?: number | string;
  reasoning_effort?: string;
  steps_limit?: number;
}

export interface LLMModelSelectorProps {
  selectedModel?: LLMModel | null;
  onSelectModel?: (model: LLMModel) => void;
  models?: LLMModel[];
  disabled?: boolean;
  onClickSettings?: () => void;
  llmSettings?: LLMSettingsValues;
  onSetLLMSettings?: (settings: LLMSettingsValues) => void;
  showStepsLimit?: boolean;
  showSettingsEntry?: boolean;
  modelTooltip?: string;
  settingsTooltip?: string;
  onResetToDefaults?: () => void;
  dataTourTargetId?: string;
}
