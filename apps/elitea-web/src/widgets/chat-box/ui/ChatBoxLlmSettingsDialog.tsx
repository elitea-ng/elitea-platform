/**
 * A14 (ELITEA-0386, "Not Published Agent in Chat — Edit LLM Settings After
 * Version Switch — No Spurious Error"): the entry point for editing an
 * agent/pipeline PARTICIPANT's own `llm_settings` from inside a live chat
 * conversation. `entities/participant`'s `updateParticipantLlmSettings` had
 * zero callers anywhere in `src/` before this file — this is that wiring,
 * split out of `ChatBox.tsx` to stay under its §3.5 file-length budget
 * (already at the 400-line ceiling), same rationale as
 * `ChatBoxContinuation.tsx`/`ChatBoxInputSlots.tsx`.
 *
 * `features/chat-input` (where `AgentEditorPanel` — the panel this dialog's
 * trigger button lives on — is built) may not import `widgets/llm-model-
 * selector` (R-L1: features may not import widgets), so `AgentEditorPanel`
 * only exposes a plain `onEditLlmSettings` callback; the dialog itself is
 * composed here, one layer up, and rendered as a sibling of `NewChatInput`
 * in `ChatBox.tsx`.
 */
import type { ReactNode } from 'react';
import { useCallback, useState } from 'react';

import type { Participant } from '@/entities/participant';
import { useUpdateParticipantLlmSettingsMutation } from '@/entities/participant';
import { LLMSettingsDialog } from '@/widgets/llm-model-selector';

/** `Participant.entitySettings` (camelCase) -> the snake_case wire shape `updateParticipantLlmSettings`'s `currentEntitySettings` needs — the same fields `useChatBoxVersioning.ts`'s `buildVersionSettings` already writes on a version switch, so this edit does not clobber them. */
function toWireEntitySettings(participant: Participant | undefined): Record<string, unknown> {
  const settings = participant?.entitySettings;
  if (!settings) return {};
  return {
    ...(settings.versionId !== undefined ? { version_id: settings.versionId } : {}),
    ...(settings.variables !== undefined ? { variables: settings.variables } : {}),
    ...(settings.iconMeta !== undefined ? { icon_meta: settings.iconMeta } : {}),
  };
}

export interface UseChatBoxLlmSettingsDialogParams {
  readonly projectId: string | number | undefined;
  readonly conversationId: string | number | undefined;
  /** The participant `onEditLlmSettings` opens the dialog for — `AgentEditorPanel`'s own `activeParticipant`/`participantForEditor`. */
  readonly participant: Participant | undefined;
}

export interface UseChatBoxLlmSettingsDialogResult {
  /** Wired to `AgentEditorPanel`'s `onEditLlmSettings` prop. */
  readonly onEdit: () => void;
  /** Rendered as a sibling of `NewChatInput` in `ChatBox.tsx`. */
  readonly dialog: ReactNode;
}

export function useChatBoxLlmSettingsDialog(params: UseChatBoxLlmSettingsDialogParams): UseChatBoxLlmSettingsDialogResult {
  const { projectId, conversationId, participant } = params;
  const [open, setOpen] = useState(false);
  const { mutate } = useUpdateParticipantLlmSettingsMutation();

  const onEdit = useCallback(() => setOpen(true), []);
  const onCancel = useCallback(() => setOpen(false), []);

  const onApply = useCallback(
    (llmSettings: Record<string, unknown>) => {
      if (projectId === undefined || conversationId === undefined || !participant) return;
      mutate(
        {
          projectId,
          conversationId: String(conversationId),
          participantId: participant.id,
          currentEntitySettings: toWireEntitySettings(participant),
          llm_settings: llmSettings,
        },
        { onSuccess: () => setOpen(false) },
      );
    },
    [projectId, conversationId, participant, mutate],
  );

  return {
    onEdit,
    dialog: (
      <LLMSettingsDialog
        open={open && !!participant}
        onApply={onApply}
        onCancel={onCancel}
        llmSettings={participant?.entitySettings?.llmSettings ?? {}}
        showStepsLimit={false}
      />
    ),
  };
}
