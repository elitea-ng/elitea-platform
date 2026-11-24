import type { ConfigurationTabProps } from '@/features/pipelines';
import { areAgentLlmSettingsEqual, type AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import { PIPELINE_STARTER_TEMPLATE } from '@/shared/lib/pipelineStarterTemplate';

export const CREATE_DRAFT_VERSION_ID = 'create-draft';

export interface CreatePipelineFormExtraFields {
  readonly instructions: string;
  readonly welcomeMessage: string;
  readonly variables: { readonly name: string; readonly value: string }[];
  readonly stepLimit: number | undefined;
  readonly llmSettings: AgentLlmSettings | undefined;
}

export function areExtraFieldsEqual(
  a: CreatePipelineFormExtraFields,
  b: CreatePipelineFormExtraFields,
): boolean {
  if (a.instructions !== b.instructions) return false;
  if (a.welcomeMessage !== b.welcomeMessage) return false;
  if (a.stepLimit !== b.stepLimit) return false;
  if (!areAgentLlmSettingsEqual(a.llmSettings, b.llmSettings)) return false;
  if (a.variables.length !== b.variables.length) return false;
  return a.variables.every((variable, index) => {
    const other = b.variables[index];
    return other !== undefined && variable.name === other.name && variable.value === other.value;
  });
}

export function pipelineInstructions(authored: string): string {
  return authored.trim() === '' ? PIPELINE_STARTER_TEMPLATE : authored;
}

/**
 * No-op chat adapter for the unsaved Create workspace: chat is locked until the
 * pipeline is saved, so none of these methods are invoked before creation.
 */
export const NOOP_CREATE_CHAT_ADAPTER: ConfigurationTabProps['adapter'] = {
  createConversation: () => Promise.resolve({}),
  deleteMessage: () => Promise.resolve({}),
  deleteAllMessages: () => Promise.resolve({}),
  stopChatTask: () => Promise.resolve(),
};
