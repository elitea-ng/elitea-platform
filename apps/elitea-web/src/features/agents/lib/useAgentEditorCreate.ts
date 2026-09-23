import { useCallback, useState } from 'react';

import { useCreateApplicationDraft, type ApplicationDraftInput } from '@/entities/application-form';
import { LATEST_VERSION_NAME } from '@/entities/version';
import type { ApplicationCreatedResponse } from '@/shared/api/generated/model';

import { setFieldValueAtPath } from './agentFieldChange';
import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';

import type { AgentDraftValues, AgentFieldChange } from '../model/types';

/**
 * Owns the create-mode form state `pages/NewChat/AgentEditor.jsx`'s own
 * `AgentEditor` component read off `useFormikContext()` (baseline has no
 * local state of its own for this — a `<Formik>` ancestor, not built by
 * this sub-unit, owned it). This app has no Formik (see
 * `../model/types.ts`'s own doc comment); `AgentEditor.tsx` is this app's
 * one real consumer of `../ui/CreateAgentForm.tsx`'s `values`/
 * `onFieldChange` contract for the CREATE path, so it is the natural owner
 * of the state those props read/write.
 *
 * **`conversation_starters` gap, disclosed:** `AgentVersionDetails`
 * (`../model/types.ts`) carries no `conversation_starters` field — matches
 * `CreateAgentForm.tsx`'s own doc comment: its `conversationStartersSlot`
 * (the baseline's `ConversationStarters.jsx`) has not landed in this
 * worktree either, so there is no live UI to source a value from yet. The
 * created agent's `conversation_starters` is therefore always `[]` via
 * this path today — a real, disclosed gap, not a silently dropped field.
 *
 * **`isDirty`/`reset`:** the baseline's ambient Formik ancestor gave
 * `AgentEditor.jsx` both "has anything changed" (`formik.dirty`, read
 * implicitly by whatever consumed its own local `isDirty` state) and
 * "restore the mounted form to its initial values" (`handleDiscard`,
 * `AgentEditor.jsx:270-273`) for free, for whichever form happened to be
 * mounted — including the create-mode `CreateAgentForm`. Since this hook is
 * the one place CREATE-mode values actually live in this app (no ambient
 * form context — see this file's own doc comment above), it is the only
 * place that can answer either question for create mode: `isDirty` is a
 * plain "has `onFieldChange` ever fired since the last successful create or
 * reset" flag (not a deep-equal diff against `EMPTY_CREATE_VALUES` — the
 * `AgentEditor.tsx` caller's `handleDiscard` doc comment). `reset` restores
 * `values` to `EMPTY_CREATE_VALUES` — CREATE mode's initial values are
 * always this fixed constant (never derived from existing agent data, same
 * as the baseline's own `createInitialValues`), so "discard" and "start a
 * fresh create form" are the identical operation.
 *
 * **`meta.internal_tools` default -- empty, matching a fresh form.** The
 * admission gates now admit the platform's whole authorable catalogue (the
 * `NOT IN` lists in `services/elitea-main/internal/db/queries/agent_chat.sql`
 * — nine sites, pinned against the other catalogue copies by
 * `internal_tools_catalogue_drift_test.go`), and the native runtime SKIPS
 * catalogue names it does not implement with a logged
 * `agent_internal_tool_skipped` (`services/elitea-worker-rust/src/agents/
 * internal_tools.rs`), so a toggled tool no longer refuses the turn. The
 * default stays empty because a fresh agent has no tools toggled — the
 * Tools panel writes the real values. `resolveInternalTools` still respects
 * an explicit override. The pipelines side
 * (`features/pipelines/lib/usePipelineEditorCreate.ts`) seeds `[]` the same
 * way.
 */
const DEFAULT_INTERNAL_TOOLS: readonly string[] = [];

function resolveInternalTools(meta: NonNullable<AgentDraftValues['version_details']>['meta']): readonly string[] {
  const raw = meta?.['internal_tools'];
  if (Array.isArray(raw) && raw.every((entry): entry is string => typeof entry === 'string')) {
    return raw;
  }
  return DEFAULT_INTERNAL_TOOLS;
}

/**
 * The model the form's picker wrote, if any. A function for the same reason
 * `resolveInternalTools` above is one: it keeps the optional-chain branch off
 * `buildCreateDraft`'s own cyclomatic complexity, which is at this codebase's
 * oxlint budget (12).
 */
function resolveLlmSettings(
  versionDetails: AgentDraftValues['version_details'],
): AgentLlmSettings | undefined {
  return versionDetails?.llm_settings;
}

export function useAgentEditorCreate(projectId: string | undefined) {
  const [values, setValues] = useState<AgentDraftValues>(EMPTY_CREATE_VALUES);
  const [isDirty, setIsDirty] = useState(false);
  const { create, isCreating, error } = useCreateApplicationDraft(projectId);

  const onFieldChange: AgentFieldChange = useCallback((path, value) => {
    setValues((prev) => setFieldValueAtPath(prev, path, value));
    setIsDirty(true);
  }, []);

  const reset = useCallback(() => {
    setValues(EMPTY_CREATE_VALUES);
    setIsDirty(false);
  }, []);

  const submit = useCallback(async (): Promise<ApplicationCreatedResponse | undefined> => {
    const result = await create(buildCreateDraft(values));
    if (result) setIsDirty(false);
    return result;
  }, [values, create]);

  return { values, onFieldChange, submit, isCreating, error, isDirty, reset };
}

const EMPTY_CREATE_VALUES: AgentDraftValues = {
  name: '',
  description: '',
  version_details: {
    instructions: '',
    welcome_message: '',
    tags: [],
    variables: [],
    tools: [],
    meta: { step_limit: 25 },
  },
};

/**
 * The optional keys of the version body, split from `buildCreateDraft` to
 * keep each function under the oxlint complexity budget (12) — the welcome
 * message spread pushed the single function to 14.
 */
function optionalCreateVersionFields(versionDetails: AgentDraftValues['version_details']) {
  // The draft carries `welcomeMessage` now (it used to have no key for it,
  // and this path silently dropped what the form collected). Spread, not
  // assigned: the key is optional-without-undefined
  // (exactOptionalPropertyTypes).
  return versionDetails?.welcome_message !== undefined ? { welcomeMessage: versionDetails.welcome_message } : {};
}

/**
 * The chat starters the form collected (#940 A12).
 *
 * This used to be a literal `[]` at the call site below, while
 * `ConversationStartersEditor` wrote `version_details.conversation_starters`
 * and `useCreateAgentFormState` carried the setter for it — the dead-wiring
 * shape where both halves are correct and nothing joins them. A user who added
 * four chat starters before saving got an agent with none and no error
 * anywhere.
 *
 * Its own function purely to keep `buildCreateDraft` under the oxlint
 * complexity budget (12), which the added optional-chain branch tipped over.
 */
function draftConversationStarters(versionDetails: AgentDraftValues['version_details']): readonly string[] {
  return [...(versionDetails?.conversation_starters ?? [])];
}

/** `submit`'s request-body construction, extracted purely to keep `useAgentEditorCreate` under the oxlint complexity budget. */
function buildCreateDraft(values: AgentDraftValues): ApplicationDraftInput {
  const versionDetails = values.version_details;
  // Read once and reused by both `step_limit` and `resolveInternalTools`
  // below -- keeps this function's own cyclomatic complexity under the
  // oxlint budget (12) by collapsing what would otherwise be two separate
  // `versionDetails?.meta` optional-chain branches into one.
  const meta = versionDetails?.meta;
  return {
    name: (values.name ?? '').trim(),
    description: values.description ?? '',
    type: 'interface',
    version: {
      name: LATEST_VERSION_NAME,
      agentType: undefined,
      instructions: versionDetails?.instructions ?? '',
      ...optionalCreateVersionFields(versionDetails),
      conversationStarters: draftConversationStarters(versionDetails),
      variables: (versionDetails?.variables ?? []).map((variable) => ({ name: variable.name, value: variable.value })),
      meta: { step_limit: meta?.step_limit ?? 25, internal_tools: resolveInternalTools(meta) },
      // Whatever the model picker wrote into the form, or `undefined` when
      // the user left it alone — `toVersionWriteRequest` then omits the key
      // and the new agent runs on the project's catalogue default.
      llmSettings: resolveLlmSettings(versionDetails),
      tags: [...(versionDetails?.tags ?? [])],
      tools: [],
      pipelineSettings: undefined,
    },
  };
}
