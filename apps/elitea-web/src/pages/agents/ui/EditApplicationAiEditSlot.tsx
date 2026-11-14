import type { ReactNode } from 'react';
import { useMemo } from 'react';

import { EditInstructionsWithAiButton } from '@/features/agents';
import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';

/**
 * The agent editor's "Edit with AI" slot.
 *
 * A separate file only so `EditApplication.tsx` stays inside its `max-lines`
 * and cyclomatic-complexity budgets — same reason `EditApplicationActions`/
 * `EditApplicationSaveBar`/`EditApplicationToolsPanel` are their own files.
 * It owns no decision: the button gates itself on the backend capability, a
 * resolvable Service Prompt and a configured model (see
 * `features/agents`' `EditInstructionsWithAiButton`), so this page mounts it
 * unconditionally and it renders nothing where the feature cannot work.
 */
/**
 * The page's editor bridge, narrowed to the two things this slot reads and
 * the one setter it writes. Structural, not the bridge's own type: the slot
 * has no business with the rest of it.
 */
interface EditApplicationAiEditEditor {
  readonly values: {
    readonly name?: string | undefined;
    readonly description?: string | undefined;
    readonly version_details: {
      readonly instructions?: string | undefined;
      readonly welcome_message?: string | undefined;
      readonly conversation_starters?: readonly string[] | undefined;
      readonly llm_settings?: AgentLlmSettings | undefined;
    };
  };
  readonly onFieldChange: (path: string, value: unknown) => void;
}

export interface EditApplicationAiEditSlotProps {
  readonly editor: EditApplicationAiEditEditor;
  readonly projectId: string | undefined;
  readonly disabled: boolean;
}

export function EditApplicationAiEditSlot({ editor, projectId, disabled }: EditApplicationAiEditSlotProps): ReactNode {
  const { name, description, version_details: version } = editor.values;
  const { instructions, welcome_message, conversation_starters, llm_settings } = version;
  const current = useMemo(
    () => ({
      name: name ?? '',
      description: description ?? '',
      instructions: instructions ?? '',
      welcome_message: welcome_message ?? '',
      conversation_starters: conversation_starters ?? [],
    }),
    [name, description, instructions, welcome_message, conversation_starters],
  );
  // Each changed field lands in ITS field, through the same `onFieldChange`
  // every other edit uses, so an applied edit reaches the save body AND the
  // dirty comparison the nav blocker reads (#133). The modal passes only the
  // fields the review changed, and never a blank name.
  const apply = (patch: Partial<typeof current>): void => {
    if (patch.name !== undefined) editor.onFieldChange('name', patch.name);
    if (patch.description !== undefined) editor.onFieldChange('description', patch.description);
    if (patch.instructions !== undefined) editor.onFieldChange('version_details.instructions', patch.instructions);
    if (patch.welcome_message !== undefined) editor.onFieldChange('version_details.welcome_message', patch.welcome_message);
    if (patch.conversation_starters !== undefined) editor.onFieldChange('version_details.conversation_starters', patch.conversation_starters);
  };
  return (
    <EditInstructionsWithAiButton
      projectId={projectId}
      current={current}
      llmSettings={llm_settings}
      onApply={apply}
      disabled={disabled}
    />
  );
}
