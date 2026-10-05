import type { ReactNode } from 'react';
import { useState } from 'react';

import AutoAwesomeOutlinedIcon from '@mui/icons-material/AutoAwesomeOutlined';

import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import { useAiEditAvailability } from '../../model/useAiEditAvailability';
import { EditInstructionsWithAiModal } from './EditInstructionsWithAiModal';

/**
 * Port of `apps/elitea-ui/src/[fsd]/entities/edit-entity-with-ai/ui/
 * EditEntityButton.jsx` + `features/agent/ui/ai-edit-agent-modal/
 * AIEditAgentButton.jsx` — the "Edit with AI" trigger, mounted where the
 * baseline mounts it (the agent's General panel, `ApplicationEditForm.jsx:97`'s
 * `summaryAction`).
 *
 * **IT RENDERS NOTHING WHEN THE FEATURE CANNOT WORK**, which in a default
 * install is always. `useAiEditAvailability` is the gate and its own doc
 * comment states each condition; the short version is: the `predict_llm`
 * route is not registered by this backend, the Service Prompt behind the
 * edit lives on `/configurations/*` (off unless `ELITEA_CONFIGURATIONS_ENABLED`),
 * and the agent version must actually name a model. The baseline gates on a
 * PERMISSION instead (`useCheckPermission`); a permission check would pass
 * here and the button would then fail on every click, which is the defect
 * class this branch exists to remove. When the backend lands, flip
 * `aiGeneration` in `shared/config/backendCapabilities.ts` — the same switch
 * `GenerateAgentButton` already waits on.
 */
export interface EditInstructionsWithAiButtonProps {
  readonly projectId: string | undefined;
  readonly instructions: string;
  readonly llmSettings: AgentLlmSettings | null | undefined;
  readonly onApply: (instructions: string) => void;
  readonly disabled?: boolean | undefined;
}

export function EditInstructionsWithAiButton(props: EditInstructionsWithAiButtonProps): ReactNode {
  const { projectId, instructions, llmSettings, onApply, disabled = false } = props;
  const [open, setOpen] = useState(false);

  const availability = useAiEditAvailability({ projectId, modelSettings: llmSettings });

  // `projectId` is re-checked here rather than trusted from `isAvailable`
  // alone so the non-optional modal prop below is narrowed by the compiler,
  // not by a comment. A version without its own `llm_settings` still edits:
  // the model is the project default (`availability.modelName`).
  if (!availability.isAvailable || projectId === undefined) return null;

  return (
    <>
      <BaseBtn
        variant="special"
        size="small"
        disabled={disabled}
        startIcon={<AutoAwesomeOutlinedIcon fontSize="small" />}
        onClick={() => setOpen(true)}
        data-testid="ai-edit-instructions-button"
      >
        {t('features.agents.aiEdit.button', 'Edit with AI')}
      </BaseBtn>
      {open && (
        <EditInstructionsWithAiModal
          open={open}
          onClose={() => setOpen(false)}
          projectId={projectId}
          instructions={instructions}
          basePrompt={availability.basePrompt}
          llmSettings={{
            // The project's CURRENT default model, not the one the version
            // was created with (legacy issue 6872).
            model_name: availability.modelName,
            temperature: llmSettings?.temperature ?? 0.7,
            max_tokens: llmSettings?.max_tokens ?? 1024,
          }}
          onApply={onApply}
        />
      )}
    </>
  );
}
