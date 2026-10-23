import { FRAME_TYPE } from '@elitea/client-core/stream';

import type { AiAssistantConfigurationTypeDescriptor } from '../api/aiAssistantConfigurations';
import type { AiAssistantLlmSettings } from '../api/aiAssistantPredict';

/**
 * Pure helpers behind `../model/useAIContentGenerationStreaming.ts`, split
 * into their own file purely to keep that file under the §3.5 400-line
 * budget (the hook itself was 413 lines with these inlined) — no baseline
 * behaviour change, see the hook's own doc comment for the full deviation
 * list this port makes from `apps/elitea-ui/src/[fsd]/features/pipelines/
 * ai-assistant/lib/hooks/useAIContentGenerationStreaming.hooks.js`.
 */

/**
 * `getAvailableConfigurationsType({section:'service_prompts'})`'s response,
 * narrowed to the `service_prompt` type's
 * `config_schema.properties.data.properties.prompt.default_by_key` path
 * the hook actually reads.
 */
export function getServicePromptDefaultsByKey(
  availableTypes: readonly AiAssistantConfigurationTypeDescriptor[] | undefined,
): Readonly<Record<string, string>> {
  const schema = availableTypes?.find((item) => item.type === 'service_prompt')?.config_schema;
  const defaults = schema?.properties?.data?.properties?.prompt?.default_by_key;
  return defaults && typeof defaults === 'object' ? defaults : {};
}

/**
 * Real wire field the baseline destructures off every `application_predict`
 * message (`stream_id`) that `shared/api/socket/events.ts`'s generated
 * `streamEnvelopeSchema` does not declare in its exported TS type (it IS
 * validated/passed-through at runtime — the schema is a deliberately loose
 * `z.looseObject`; see that file's own doc comment). Narrowed locally,
 * matching this codebase's `[key: string]: unknown`-passthrough convention
 * for genuinely dynamic wire payloads (e.g. `features/credentials/api/
 * configurations.ts`'s `ConfigSchemaNode`).
 */
export interface ApplicationPredictStreamMessage {
  readonly type: string;
  readonly stream_id?: string;
  readonly content?: unknown;
  // Explicit `| undefined`: the generated `streamEnvelopeSchema` type
  // (`shared/api/socket/events.ts`) types this as `Record<string, unknown>
  // | undefined`, not a plain optional — under `exactOptionalPropertyTypes`
  // a bare `response_metadata?: Readonly<Record<string, unknown>>` target
  // rejects that value even though omitting the field entirely is fine.
  // Same fix `shared/ui/CodeMirrorEditor.tsx`'s `readOnly?: boolean |
  // undefined` doc comment documents for the identical class of mismatch.
  readonly response_metadata?: Readonly<Record<string, unknown>> | undefined;
  readonly [key: string]: unknown;
}

/** `SocketMessageType`'s string literals the baseline switches on (`common/constants.js`), taken from `@elitea/client-core`'s `FRAME_TYPE`. */
export const START_TASK = FRAME_TYPE.StartTask;
export const CHUNK = FRAME_TYPE.Chunk;
export const AI_MESSAGE_CHUNK = FRAME_TYPE.AIMessageChunk;
export const AGENT_LLM_CHUNK = FRAME_TYPE.AgentLlmChunk;
export const AGENT_RESPONSE = FRAME_TYPE.AgentResponse;
export const AGENT_LLM_END = FRAME_TYPE.AgentLlmEnd;
export const SOCKET_ERROR = FRAME_TYPE.Error;
export const LLM_ERROR = FRAME_TYPE.LlmError;

// Backstop: if no completion event arrives (events dropped, network drop,
// backend silently aborted), unblock the UI so the user is not stuck on
// "Thinking..." forever.
export const SAFETY_TIMEOUT_MS = 60_000;
export const FLUSH_KEEP_ALIVE_MS = 5000;

export function convertSocketContent(content: unknown): string {
  if (content == null) return '';
  if (typeof content === 'string') return content;
  return JSON.stringify(content);
}

/** The 3 precondition checks `generateContent` runs before starting a stream, collapsed to one `if` at the call site — split out to keep that function's cyclomatic complexity under the §3.5 budget (12). */
export function readGenerationBlocker(
  socketId: string | undefined,
  modelName: string | undefined,
  projectId: string | number | undefined,
): string | null {
  if (!socketId) return 'Socket connection not available';
  if (!modelName) return 'No LLM model configured. Please configure a model in the pipeline settings.';
  if (projectId === undefined) return 'No project selected.';
  return null;
}

/** `llm_settings` construction, split out for the same complexity-budget reason as {@link readGenerationBlocker} (the `exactOptionalPropertyTypes`-safe conditional spread for `integration_uid` counts as a branch too). */
export function buildLlmSettings(modelConfig: AiAssistantLlmSettings): AiAssistantLlmSettings {
  return {
    model_name: modelConfig.model_name,
    ...(modelConfig.integration_uid !== undefined ? { integration_uid: modelConfig.integration_uid } : {}),
    temperature: modelConfig.temperature ?? 0.7,
    max_tokens: modelConfig.max_tokens ?? 1024,
  };
}
