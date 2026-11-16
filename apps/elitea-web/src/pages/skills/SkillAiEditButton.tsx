import type { ReactNode } from 'react';
import { useMemo } from 'react';

import { EditInstructionsWithAiButton } from '@/features/agents';
import { isSkillValid, type SkillWriteInput } from '@/features/skills';

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
 * Apply writes back only the three fields a skill has, each only if it passes
 * the skill's own limits (a returned name that is blank or too long is dropped).
 * The prompt itself is told it is editing a skill, not an agent.
 *
 * A skill version carries no model of its own, so the project's default model
 * is used (`llmSettings: null`).
 */
/** A proposed field value, applied only when the skill's own rules accept it; the field keeps its text otherwise. */
function accepted(field: 'name' | 'description' | 'instructions', proposed: string | undefined, current: string): string {
  if (proposed === undefined) return current;
  const probe = { name: 'n', description: 'd', instructions: 'i', tags: [] as readonly string[], [field]: proposed };
  return isSkillValid(probe) ? proposed : current;
}

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
          name: accepted('name', patch.name, value.name),
          description: accepted('description', patch.description, value.description),
          instructions: accepted('instructions', patch.instructions, value.instructions),
        })
      }
    />
  );
}
