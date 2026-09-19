import type { PipelineTriggerScope } from '../lib/flow-editor/flowEditorContext';
import type { ChatPipelineVersionDetails } from '../lib/hooks/pipelineChat.types';

/**
 * Pure helpers lifted out of `ConfigurationTab.tsx` — that file sits one line
 * under the §3.5 400-line budget, and #899 needed room there for the trigger
 * scope provider. Nothing here changed in the move.
 */

interface ToolLike {
  readonly type?: string;
  readonly id?: string;
}

/** `ConfigurationTab.jsx`'s `existingToolkitIds` derivation — a pure function so the component's own cyclomatic complexity stays under this codebase's gate. */
export function extractExistingToolkitIds(tools: readonly unknown[] | undefined): readonly string[] {
  return (tools ?? [])
    .filter((tool): tool is ToolLike & { readonly id: string } => (tool as ToolLike)?.type === 'toolkit' && typeof (tool as ToolLike)?.id === 'string')
    .map((tool) => tool.id);
}

/** #899: what the Entrypoint node's Trigger surface addresses — this project and the version on screen. */
export function triggerScope(projectId: string | undefined, versionDetails: ChatPipelineVersionDetails | undefined): PipelineTriggerScope {
  return {
    projectId,
    versionId: versionDetails?.id === undefined ? undefined : Number(versionDetails.id),
    versionInstructions: versionDetails?.instructions,
  };
}
