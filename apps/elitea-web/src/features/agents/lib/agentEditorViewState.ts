/**
 * Pure derivations extracted from `pages/NewChat/AgentEditor.jsx`'s
 * top-level component body (lines 128-143) so `ui/AgentEditor.tsx` can stay
 * under the §3.5 per-function complexity budget and so this logic is
 * directly unit-testable without mounting the whole editor.
 */

import { canEditApplication } from '@/shared/lib/applicationEditAccess';

export const PUBLIC_PROJECT_ID = 'public';

export interface AgentEditorAgentLike {
  readonly id?: string | number;
  readonly entity_meta?: { readonly id?: string | number; readonly project_id?: string | number };
  readonly entity_settings?: { readonly version_id?: string | number };
  readonly meta?: { readonly id?: string | number; readonly name?: string };
  readonly name?: string;
}

/** `getAgentId` — `agent?.entity_meta?.id || agent?.id || agent?.meta?.id` — `AgentEditor.jsx:25-28`. */
export function agentId(agent: AgentEditorAgentLike | null | undefined): string | number | undefined {
  return agent?.entity_meta?.id || agent?.id || agent?.meta?.id;
}

/** `agent?.entity_meta?.project_id === PUBLIC_PROJECT_ID` — `AgentEditor.jsx:132`. */
export function isPublicAgent(agent: AgentEditorAgentLike | null | undefined): boolean {
  return agent?.entity_meta?.project_id === PUBLIC_PROJECT_ID;
}

/** `!isPublic && hasEditPermission` — `AgentEditor.jsx:133`. */
export function canEditAgent(isPublic: boolean, hasEditPermission: boolean): boolean {
  return canEditApplication(isPublic, hasEditPermission);
}

/** The baseline's `ViewMode.Owner`/`ViewMode.Public` string literals (`common/constants.js`) — `AgentEditor.jsx:134`. */
export function agentViewMode(canEditIt: boolean): 'Owner' | 'Public' {
  return canEditIt ? 'Owner' : 'Public';
}

/** `agent?.meta?.name || agent?.name || fallback` — the editor title/name-fallback chain used at `AgentEditor.jsx:200,235,312`. */
export function agentDisplayName(agent: AgentEditorAgentLike | null | undefined, fallback = ''): string {
  return agent?.meta?.name || agent?.name || fallback;
}

/**
 * `isPublic && onConversationLlmOverride ? onConversationLlmOverride : undefined`
 * (`AgentEditor.jsx:348-349`) — a per-conversation LLM override only ever
 * applies to a PUBLIC agent; gating it here (rather than inline in
 * `AgentEditor.tsx`) keeps that file's own per-function complexity down,
 * same reason every other export in this module exists.
 */
export function publicLlmOverride<T>(isPublic: boolean, onConversationLlmOverride: T | undefined): T | undefined {
  return isPublic ? onConversationLlmOverride : undefined;
}

/** `canEditIt || !!onPublicLlmOverride` (`AgentEditor.jsx:62`) — the model selector is editable when the viewer can edit the whole agent, or a caller opted into a per-conversation override for it. */
export function canEditModel(canEditIt: boolean, hasConversationLlmOverride: boolean): boolean {
  return canEditIt || hasConversationLlmOverride;
}

/**
 * `entityProjectId || projectId` (`AgentEditor.jsx:68`) — the agent's own
 * owning project, used as-is whenever truthy regardless of its type,
 * falling back to the globally-selected project only when the agent
 * doesn't carry one (create mode, or a legacy/public agent with no
 * `entity_meta.project_id`). Stringified since `ApplicationValidator`
 * wants a `string`, whatever `entityProjectId`'s own runtime type.
 */
export function resolveValidateProjectId(
  entityProjectId: string | number | undefined,
  projectId: string | undefined,
): string | undefined {
  return entityProjectId ? String(entityProjectId) : projectId;
}
