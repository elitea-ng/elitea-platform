/**
 * The pipeline editor's create-mode form body, for the chat composition root.
 *
 * Its own file rather than a third function in `EditorShell.tsx`: that file is
 * at its §3.5 400-line budget, and this is a different concern anyway — the
 * shell is the editor CHROME (header, Save/Discard/Close, the scroll body),
 * while this is the form that goes inside it.
 */
import type { ReactNode } from 'react';

import { CreateAgentForm } from '@/features/agents';
import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';

/**
 * The create-mode form body `PipelineEditor` asks for through
 * `deps.renderCreateForm` (#940 A12).
 *
 * `AgentEditor` renders `CreateAgentForm` itself; `PipelineEditor` cannot —
 * `CreateAgentForm` is `features/agents`-owned and `no-sideways-features`
 * forbids `features/pipelines` reaching it, which is precisely why that slot
 * exists (`PipelineEditor.tsx`'s own module doc names the baseline call site:
 * `CreateAgentForm entityType="pipeline" showInstructions={false}`,
 * PipelineEditor.jsx:517-523). `processes/` may import both slices, so this is
 * the layer where the two meet.
 *
 * UNTIL THIS FUNCTION EXISTED THE SLOT WAS EMPTY. `ChatWithEditors` passed
 * `deps={{ renderShell }}` and nothing else, so "Create new" under the
 * composer's Pipelines submenu opened an editor with a title, a Save button
 * and NO FORM — no name, no description, nothing to fill in, and a Save that
 * could only ever post an unnamed pipeline. The agent half of the same menu
 * worked, which is what made the gap easy to miss.
 *
 * `showInstructions={false}` matches the baseline and is load-bearing here:
 * a pipeline's `instructions` IS its YAML graph (`usePipelineEditorCreate`'s
 * own comment), seeded from the starter template, and exposing it as a free
 * text field at create time lets a user replace the graph with prose and
 * store a pipeline no runtime can compile.
 *
 * `values` is a `PipelineDraftValues`, a structural subset of the
 * `AgentDraftValues` this form declares (`features/pipelines/model/types.ts`
 * says so directly), so it is passed straight through with no adapter.
 */
export function renderPipelineCreateForm(props: PipelineCreateFormSlotLikeProps): ReactNode {
  return (
    <CreateAgentForm
      values={props.values}
      onFieldChange={props.onFieldChange}
      entityType="pipeline"
      showInstructions={false}
    />
  );
}

/**
 * `PipelineCreateFormSlotProps` structurally, declared locally for the same
 * reason `EditorShellRenderProps` is: the real type is not exported from
 * `features/pipelines`' barrel, and TypeScript checks the assignment
 * structurally at the `deps.renderCreateForm` call site.
 */
export interface PipelineCreateFormSlotLikeProps {
  readonly values: {
    readonly id?: number | undefined;
    readonly name?: string | undefined;
    readonly description?: string | undefined;
    /**
     * The field set is the INTERSECTION both real draft types already share
     * — `PipelineVersionDetails` must be assignable TO this (the slot's own
     * parameter, checked contravariantly at the `deps.renderCreateForm` call
     * site) and this must be assignable to `AgentVersionDetails` (what
     * `CreateAgentForm` declares). An index signature satisfies neither
     * direction, which is why the keys are written out.
     */
    readonly version_details?:
      | {
          readonly id?: number | undefined;
          readonly instructions?: string | undefined;
          readonly welcome_message?: string | undefined;
          readonly conversation_starters?: readonly string[] | undefined;
          readonly tags?: readonly string[] | undefined;
          readonly variables?: readonly { readonly id?: string | number | undefined; readonly name: string; readonly value: string }[] | undefined;
          readonly meta?: { readonly step_limit?: number | undefined } | undefined;
          readonly llm_settings?: AgentLlmSettings | undefined;
        }
      | undefined;
  };
  readonly onFieldChange: (path: string, value: unknown) => void;
}
