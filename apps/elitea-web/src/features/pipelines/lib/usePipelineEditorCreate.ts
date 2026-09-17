import { useCallback, useState } from 'react';

import { useCreateApplicationDraft, type ApplicationDraftInput } from '@/entities/application-form';
import { LATEST_VERSION_NAME } from '@/entities/version';
import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import type { ApplicationCreatedResponse } from '@/shared/api/generated/model';

import { setFieldValueAtPath } from './pipelineFieldChange';
import { PIPELINE_STARTER_TEMPLATE } from '@/shared/lib/pipelineStarterTemplate';
import type { PipelineDraftValues, PipelineFieldChange } from '../model/types';

/**
 * Owns the create-mode form state `pages/NewChat/PipelineEditor.jsx`'s own
 * `PipelineEditor` component read off `useFormikContext()` (baseline has no
 * local state of its own — a `<Formik>` ancestor, not built by this
 * sub-unit, owned it; baseline's create-mode branch renders `CreateAgentForm
 * entityType="pipeline" showInstructions={false}`, `PipelineEditor.jsx:
 * 517-523`). This app has no Formik (`../model/types.ts`'s own doc
 * comment); `ui/PipelineEditor.tsx` is this app's one consumer of a
 * `values`/`onFieldChange`-shaped create form (mirroring
 * `features/agents/lib/useAgentEditorCreate.ts`'s exact same role for
 * `AgentEditor.tsx`), so it is the natural owner of the state those props
 * would read/write.
 *
 * `agentType: 'pipeline'` is always sent (baseline:
 * `useApplicationInitialValues.jsx`'s pipeline branch,
 * `entities/application-form/model/initialValues.ts`'s own
 * `forPipeline ? 'pipeline' : undefined` — the create-mode equivalent of
 * that same discriminant). `pipelineSettings` is always `{ nodes: [], edges:
 * [] }` on create — a brand-new pipeline has no flow graph yet, matching
 * `useCreateApplicationInitialValues(true)`'s own seed.
 *
 * **`conversation_starters`/`welcome_message` — CLOSED by #940 A12.** This
 * comment used to record them as a disclosed gap ("no live UI for either has
 * landed in this worktree"). There is one: `processes/chat/ui/EditorShell.tsx`'s
 * `renderPipelineCreateForm` mounts the same `CreateAgentForm entityType=
 * "pipeline"` the baseline mounts, at the one layer allowed to import both
 * slices, and it renders `WelcomeMessageInput` and `ConversationStartersEditor`
 * for a pipeline exactly as it does for an agent. `submit` now carries what
 * they write; sending `[]` regardless is what made the panels look decorative.
 *
 * **`meta.internal_tools` default — empty, matching a fresh form.** The
 * admission gates admit the platform's whole authorable catalogue now (the
 * nine `NOT IN` sites in `services/elitea-main/internal/db/queries/
 * agent_chat.sql`, pinned against the other catalogue copies by
 * `internal_tools_catalogue_drift_test.go`), and the native runtime skips
 * catalogue names it does not implement with a logged
 * `agent_internal_tool_skipped` — a toggled tool no longer refuses the turn.
 * The default stays empty because a fresh pipeline has no tools toggled;
 * the Tools panel writes the real values. `resolveInternalTools` still
 * respects an explicit override. `features/agents/lib/useAgentEditorCreate.ts`
 * is the agent-side mirror of this hook, with the same seed.
 */
const DEFAULT_INTERNAL_TOOLS: readonly string[] = [];

/**
 * Split out purely so `submit`'s own cyclomatic complexity stays under this
 * codebase's oxlint budget (12) — takes the already-resolved `meta` object
 * (one `?.` at the `submit` call site, same as the existing `step_limit`
 * read) rather than the raw `unknown` value, so the `internal_tools` key
 * lookup's own optional-chain branch is counted against THIS function, not
 * `submit`. See this module's own doc comment for why the default is empty
 * and why an explicit value still wins over it.
 */
/** The model the form's picker wrote, if any — split out for the same complexity reason as `resolveInternalTools` below. */
function resolveLlmSettings(
  versionDetails: PipelineDraftValues['version_details'],
): AgentLlmSettings | undefined {
  return versionDetails?.llm_settings;
}

function resolveInternalTools(meta: NonNullable<PipelineDraftValues['version_details']>['meta']): readonly string[] {
  const raw = meta?.['internal_tools'];
  if (Array.isArray(raw) && raw.every((entry): entry is string => typeof entry === 'string')) {
    return raw;
  }
  return DEFAULT_INTERNAL_TOOLS;
}

/**
 * The two fields the shared create form collects and this path used to send
 * neither of — #940 A12, the same dead-wiring fix the agent twin needed.
 * `welcomeMessage` is SPREAD rather than assigned because the draft key is
 * optional-without-undefined (exactOptionalPropertyTypes).
 *
 * Its own function for the oxlint complexity budget (12), the same reason
 * `resolveInternalTools`/`resolveLlmSettings` above are separate.
 */
function optionalCreateVersionFields(versionDetails: PipelineDraftValues['version_details']) {
  return versionDetails?.welcome_message !== undefined ? { welcomeMessage: versionDetails.welcome_message } : {};
}

/** The chat starters the shared create form collected — own function for the complexity budget, mirroring `useAgentEditorCreate`'s twin. */
function draftConversationStarters(versionDetails: PipelineDraftValues['version_details']): readonly string[] {
  return [...(versionDetails?.conversation_starters ?? [])];
}

/** The version's variables in write shape — own function for the same budget reason. */
function draftVariables(
  versionDetails: PipelineDraftValues['version_details'],
): readonly { readonly name: string; readonly value: string }[] {
  return (versionDetails?.variables ?? []).map((variable) => ({ name: variable.name, value: variable.value }));
}

/** `submit`'s request-body construction, extracted for the same budget reason. */
function buildPipelineCreateDraft(values: PipelineDraftValues): ApplicationDraftInput {
  const versionDetails = values.version_details;
  // Read once and reused by both `step_limit` and `resolveInternalTools`
  // below — collapses what would otherwise be two separate
  // `versionDetails?.meta` optional-chain branches into one.
  const meta = versionDetails?.meta;
  return {
    name: (values.name ?? '').trim(),
    description: values.description ?? '',
    type: 'interface',
    version: {
      name: LATEST_VERSION_NAME,
      agentType: 'pipeline',
      instructions: versionDetails?.instructions ?? '',
      ...optionalCreateVersionFields(versionDetails),
      conversationStarters: draftConversationStarters(versionDetails),
      variables: draftVariables(versionDetails),
      meta: { step_limit: meta?.step_limit ?? 25, internal_tools: resolveInternalTools(meta) },
      // Same read-through as the agents twin in `useAgentEditorCreate`:
      // absent unless the form's model picker put something there.
      llmSettings: resolveLlmSettings(versionDetails),
      tags: [...(versionDetails?.tags ?? [])],
      tools: [],
      pipelineSettings: { nodes: [], edges: [] },
    },
  };
}

export function usePipelineEditorCreate(projectId: string | undefined) {
  const [values, setValues] = useState<PipelineDraftValues>(EMPTY_CREATE_VALUES);
  const { create, isCreating, error } = useCreateApplicationDraft(projectId);

  const onFieldChange: PipelineFieldChange = useCallback((path, value) => {
    setValues((prev) => setFieldValueAtPath(prev, path, value));
  }, []);

  const submit = useCallback(async (): Promise<ApplicationCreatedResponse | undefined> => {
    return create(buildPipelineCreateDraft(values));
  }, [values, create]);

  return { values, onFieldChange, submit, isCreating, error };
}

const EMPTY_CREATE_VALUES: PipelineDraftValues = {
  name: '',
  description: '',
  version_details: {
    // `instructions` IS the pipeline's YAML graph — `usePipelineVersionSync`
    // parses it back out of the saved version and seeds the flow editor with
    // it. An empty string stored a pipeline with no graph, which no runtime
    // can run: the compiler refuses an empty document, so the first chat turn
    // failed. Ship the starter graph instead. See
    // `./pipelineStarterTemplate.ts` for why those exact keys.
    instructions: PIPELINE_STARTER_TEMPLATE,
    welcome_message: '',
    tags: [],
    variables: [],
    meta: { step_limit: 25 },
  },
};
