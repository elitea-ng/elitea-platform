export { default as LLMModelSelector } from './ui/LLMModelSelector';
export { LLMSettingsDialog } from './ui/LLMSettingsDialog';
export { LLMSettings } from './ui/LLMSettings';
/**
 * On the barrel because another slice needs them: a caller outside
 * `llm-model-selector` can only enter through this file
 * (`.dependency-cruiser.cjs`'s `no-deep-slice-import-cross-slice`), and
 * anything that feeds `LLMModelSelector` a catalogue-derived model list
 * needs both the adapter and the shapes its two data props are typed with.
 */
export { toLlmModel } from './lib/toLlmModel';
export type { LLMModel, LLMSettingsValues } from './lib/types';
/** The desktop composer's model picker (`ChatBoxComposerTools`). */
export { ComposerModelPicker } from './ui/ComposerModelPicker';
