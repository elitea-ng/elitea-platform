import { useCallback, useMemo } from 'react';

import { useWatch, type UseFormReturn } from 'react-hook-form';

import type { ApplicationCreationInput } from '@/entities/application-form';
import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';

import type { EditPipelineVersionFieldsState } from './useEditPipelineVersionFields';

/**
 * Bridges `EditPipeline`'s RHF form to `CreateAgentForm`'s plain `values`/
 * `onFieldChange` prop shape — the pipelines-domain twin of
 * `pages/agents/lib/useEditApplicationEditorBridge.ts`, and the replacement
 * for `./useEditPipelineConfigurationTabBridge.ts`'s write half.
 *
 * `useWatch`, NOT `form.watch(...)`: this page feeds `useForm({ values })` and
 * the values arrive asynchronously, after the pipeline detail resolves. A
 * render-body `form.watch()` does not re-render on that arrival, so the inputs
 * render blank over a pipeline the page's own heading already names.
 *
 * `version_details.instructions` is absent from `values` on purpose. The
 * pipeline form mounts `CreateAgentForm` with `showInstructions={false}`
 * because a pipeline's `instructions` column is its flow-graph YAML — see
 * `./useEditPipelineVersionFields.ts` for the whole reason.
 */
export interface EditPipelineEditorBridge {
  readonly values: {
    readonly name: string;
    readonly description: string;
    readonly version_details: {
      readonly id: number | undefined;
      readonly conversation_starters: readonly string[];
      readonly welcome_message: string;
      readonly variables: readonly { readonly name: string; readonly value: string }[];
      readonly meta: { readonly step_limit: number | undefined };
      /** `undefined` for a version that names no model, which the picker renders as the project's catalogue default. */
      readonly llm_settings: AgentLlmSettings | undefined;
    };
  };
  readonly onFieldChange: (path: string, value: unknown) => void;
}

export function useEditPipelineEditorBridge(
  form: UseFormReturn<ApplicationCreationInput>,
  versionFields: EditPipelineVersionFieldsState,
  versionId: number | undefined,
): EditPipelineEditorBridge {
  const name = useWatch({ control: form.control, name: 'name' }) ?? '';
  const description = useWatch({ control: form.control, name: 'description' }) ?? '';
  const conversationStarters = useWatch({ control: form.control, name: 'version_details.conversation_starters' });
  const starters = useMemo(
    () => (conversationStarters ?? []).filter((entry): entry is string => typeof entry === 'string'),
    [conversationStarters],
  );

  const { fields, applyFieldChange } = versionFields;

  const values = useMemo(
    () => ({
      name,
      description,
      version_details: {
        // `WelcomeMessageInput` re-seeds its own local draft off this id, so a
        // version switch has to change it or the previous version's message
        // stays on screen over the new one.
        id: versionId,
        conversation_starters: starters,
        welcome_message: fields.welcomeMessage,
        variables: fields.variables,
        meta: { step_limit: fields.stepLimit },
        llm_settings: fields.llmSettings,
      },
    }),
    [name, description, starters, fields, versionId],
  );

  const onFieldChange = useCallback(
    (path: string, value: unknown) => {
      if (applyFieldChange(path, value)) return;
      if (path === 'version_details.conversation_starters') {
        form.setValue(
          'version_details.conversation_starters',
          Array.isArray(value) ? value.filter((entry): entry is string => typeof entry === 'string') : [],
          { shouldValidate: true, shouldDirty: true },
        );
        return;
      }
      // `name`/`description` stay on RHF: they are the two fields
      // `applicationCreationSchema` validates and the two the Save button's
      // `formState.isValid` gate reads. Anything else is a path no control on
      // this form emits.
      if (path !== 'name' && path !== 'description') return;
      form.setValue(path, typeof value === 'string' ? value : '', { shouldValidate: true, shouldDirty: true });
    },
    [form, applyFieldChange],
  );

  return { values, onFieldChange };
}
