import { useEffect } from 'react';

import { FlowEditorConstants } from '../flow-editor/constants';
import { usePipelineYamlStore } from '../../model/pipelineYamlStore';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/lib/hooks/
 * usePipelineAttachmentYamlSync.hooks.js`.
 *
 * Watches the pipeline's internal_tools list and keeps the `input_attachments`
 * YAML state variable in sync: adds it when attachments are enabled, removes
 * it when they are disabled.
 *
 * **Disclosed deviation:** the baseline reads `useFormikContext()` for
 * `values.version_details.meta.internal_tools` and dispatches to
 * `state.pipeline` via Redux. This app has no Formik (§2.3) and this
 * store's own `usePipelineYamlStore.ts` (this sub-unit's own file) replaces
 * the Redux slice — `hasAttachments` becomes an explicit boolean parameter
 * (the caller reads it from wherever its own live form state lives, matching
 * the "ambient Formik -> explicit prop" convention this codebase already
 * establishes elsewhere, e.g. `features/agents/lib/useAgentAttachments.ts`'s
 * own identical `internalTools` parameter for the SAME underlying baseline
 * field). `dispatch(pipelineActions.setYamlCode(...))`/
 * `setYamlJsonObject(...)` become one ordered atomic store edit against current source.
 *
 * Must be called inside whatever owns the pipeline's live `internal_tools`
 * value on the pipeline configuration page (baseline: inside the Formik
 * context on `pages/Pipelines/Components/ConfigurationTab.jsx`).
 */
export function usePipelineAttachmentYamlSync(hasAttachments: boolean): void {
  const yamlCode = usePipelineYamlStore((state) => state.yamlCode);
  const editPipelineYamlDocument = usePipelineYamlStore((state) => state.editPipelineYamlDocument);

  useEffect(() => {
    try {
      editPipelineYamlDocument((currentYamlObj) => {
        const authoredState = currentYamlObj['state'];
        if (
          Object.hasOwn(currentYamlObj, 'state') &&
          (authoredState === null || typeof authoredState !== 'object' || Array.isArray(authoredState))
        )
          throw new Error('Expected an attachment state mapping');
        const currentState = (authoredState as Readonly<Record<string, unknown>> | undefined) ?? {
          ...FlowEditorConstants.DefaultState,
        };
        const alreadyHasKey = Object.hasOwn(currentState, FlowEditorConstants.STATE_INPUT_ATTACHMENTS);
        if (hasAttachments && !alreadyHasKey) {
          return {
            ...currentYamlObj,
            state: {
              ...currentState,
              [FlowEditorConstants.STATE_INPUT_ATTACHMENTS]: {
                type: FlowEditorConstants.StateVariableTypes.List,
                value: [],
              },
            },
          };
        }
        if (!hasAttachments && alreadyHasKey) {
          const remainingState = Object.fromEntries(
            Object.entries(currentState).filter(([key]) => key !== FlowEditorConstants.STATE_INPUT_ATTACHMENTS),
          );
          return { ...currentYamlObj, state: remainingState };
        }
        return currentYamlObj;
      });
    } catch {
      // Keep malformed/unsupported raw text intact; synchronization retries after a valid YAML edit.
    }
  }, [hasAttachments, yamlCode, editPipelineYamlDocument]);
}
