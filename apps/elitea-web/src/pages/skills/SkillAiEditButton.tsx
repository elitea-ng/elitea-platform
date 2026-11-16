import type { ReactNode } from 'react';
import { useMemo } from 'react';

import { EditInstructionsWithAiButton } from '@/features/agents';
import type { SkillWriteInput } from '@/features/skills';

export interface SkillAiEditButtonProps {
  readonly projectId: string | undefined;
  readonly value: SkillWriteInput;
  readonly onChange: (value: SkillWriteInput) => void;
  readonly disabled?: boolean;
}

/**
 * "Edit with AI" on the skill editor (#2104): the agent editor's own control
 * (`EditInstructionsWithAiButton`), so a skill gets the same availability gate,
 * model resolution, Service Prompt, review diff and Apply as an agent. The
 * agent-only fields (welcome message, starters) are left out of the review, and
 * Apply writes back only the three fields a skill has.
 *
 * A skill version carries no model of its own, so the project's default model
 * is used (`llmSettings: null`).
 */
export function SkillAiEditButton({ projectId, value, onChange, disabled = false }: SkillAiEditButtonProps): ReactNode {
  const current = useMemo(
    () => ({
      name: value.name,
      description: value.description,
      instructions: value.instructions,
      welcome_message: '',
      conversation_starters: [] as readonly string[],
    }),
    [value.name, value.description, value.instructions],
  );
  return (
    <EditInstructionsWithAiButton
      projectId={projectId}
      current={current}
      llmSettings={null}
      agentFields={false}
      disabled={disabled}
      onApply={(patch) =>
        onChange({
          ...value,
          name: patch.name ?? value.name,
          description: patch.description ?? value.description,
          instructions: patch.instructions ?? value.instructions,
        })
      }
    />
  );
}
