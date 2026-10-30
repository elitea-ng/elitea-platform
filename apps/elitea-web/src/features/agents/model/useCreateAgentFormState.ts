import type { ChangeEvent, FocusEvent } from 'react';
import { useCallback, useEffect, useMemo, useState } from 'react';

import type { OutputContract } from '@/shared/api/generated/model';
import { MAX_NAME_LENGTH } from '@/shared/lib/limits';
import { PROMPT_PAYLOAD_KEY } from '@/shared/lib/prompt-payload';
import { contextResolver } from '@/shared/lib/string';

import { useFieldFocus } from '../lib/useFieldFocus';
import type { AgentDraftValues, AgentFieldChange, AgentVariable } from './types';

/**
 * Split out of `ui/CreateAgentForm.tsx` into `model/` purely to keep that
 * file under the oxlint `max-lines` budget (400) — same content, moved, not
 * rewritten. Owns every piece of local state + the Formik-
 * `setFieldValue`-replacement callbacks `CreateAgentForm` needs (see
 * `../model/types.ts`'s own "DISCLOSED REDESIGN" doc comment for why these
 * are plain callbacks instead of ambient form context). A custom hook's
 * complexity is counted separately from the component that calls it — the
 * same effect `ui/CreateAgentForm.tsx`'s own `GeneralFields` split achieves
 * for JSX, applied here to state/handlers instead.
 */
export interface CreateAgentFormState {
  readonly name: string;
  readonly description: string;
  readonly nameAtMax: boolean;
  readonly nameFocused: boolean;
  readonly descriptionFocused: boolean;
  readonly variables: readonly AgentVariable[];
  readonly onChangeName: (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => void;
  readonly onNameFocus: () => void;
  readonly onNameBlur: () => void;
  readonly onDescriptionChange: (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => void;
  readonly onDescriptionFocus: () => void;
  readonly onDescriptionBlur: (event: FocusEvent<HTMLInputElement | HTMLTextAreaElement>) => void;
  readonly onChangeVariable: (label: string, newValue: string) => void;
  readonly onInstructionsChange: (value: string) => void;
  readonly onWelcomeMessageChange: (value: string) => void;
  readonly onStepLimitChange: (value: number | undefined) => void;
  /** #1500 — `meta.output_contract` as stored (opaque; `ApplicationAdvanceSettings` reads it). */
  readonly outputContract: unknown;
  /** #1500 — writes `meta.output_contract`; `null` clears it. */
  readonly onOutputContractChange: (next: OutputContract | null) => void;
  /** #307 — replaces the baseline's `setFieldValue('version_details.conversation_starters', ...)` inside `ConversationStarters.jsx` itself. */
  readonly onConversationStartersChange: (next: readonly string[]) => void;
}

/**
 * The manual-authoring half of variable AUTHORING, restored from the
 * baseline: `apps/elitea-ui/src/[fsd]/features/agent/ui/agent-details/
 * configurations/input/InstructionsInput.jsx:85-101` (`updateVariableList`),
 * fed by `shared/ui/input/FileReaderInput.jsx:62-75` on every instructions
 * edit.
 *
 * The baseline's rule, measured (not guessed) off those two files:
 *  - The variables list is DERIVED from the `{{placeholder}}`s in the
 *    instructions and REPLACED wholesale — `contextResolver(value).map(...)`
 *    with no merge of names the text no longer mentions.
 *  - A value already typed for a name that is STILL mentioned survives
 *    (`prevValue?.value || ''`, matched by name).
 *  - A name the text stops mentioning is dropped unconditionally — the
 *    baseline has no confirmation, and no "keep it because it had a value"
 *    branch. This is deliberate parity, not an oversight on this port: the
 *    row exists to fill a placeholder, so a list that could outlive its
 *    placeholders is exactly the name/instructions desync the derive-only
 *    design exists to prevent. Re-typing the placeholder brings the row
 *    back (empty).
 *  - There is no add-a-row editor anywhere in the baseline's agent form;
 *    `components/VariableDialog.jsx` is the chat-run-time surface, not this
 *    one. Authoring a variable IS typing its placeholder.
 *
 * DISCLOSED DEVIATIONS, both narrowing:
 *  - `id` is not carried across. The baseline kept `prevValue?.id`; nothing
 *    in this app reads or sends it (`pages/agents/lib/
 *    useEditApplicationVersionFields.ts`'s `fromVersion` already drops it,
 *    and both create pages type the field as `{name, value}[]`), so
 *    emitting it would widen the wire shape for no reader.
 *  - No 500ms debounce of its own. The baseline debounced because its
 *    textarea's `onChange` was per-keystroke; this app's instructions field
 *    is `shared/ui/CodeMirrorEditor`, whose `onChange` is ALREADY debounced
 *    (~30ms) upstream. A second, longer debounce here would only re-open the
 *    baseline's own race — a Save landing inside the window persists
 *    instructions whose variables were never derived.
 */
function deriveVariablesFromInstructions(
  instructions: string,
  previous: readonly AgentVariable[],
): readonly AgentVariable[] {
  return contextResolver(instructions).map((name) => ({
    name,
    value: previous.find((variable) => variable.name === name)?.value ?? '',
  }));
}

/** Split out of the hook so its optional chain does not count against the hook's complexity budget. */
function storedOutputContract(values: AgentDraftValues): unknown {
  return values.version_details?.meta?.['output_contract'];
}

/**
 * Whether the derived list is the one already held — checked so an
 * instructions keystroke that changes no placeholder emits no
 * `version_details.variables` write at all. Without it every keystroke would
 * hand each caller a fresh array: harmless on the edit page (its `areEqual`
 * compares key by key) but enough to arm the create pages' unsaved-changes
 * guard (#133) on a draft nobody has actually changed.
 */
function sameVariables(a: readonly AgentVariable[], b: readonly AgentVariable[]): boolean {
  return a.length === b.length && a.every((variable, index) => b[index]?.name === variable.name && b[index]?.value === variable.value);
}

export function useCreateAgentFormState(values: AgentDraftValues, onFieldChange: AgentFieldChange): CreateAgentFormState {
  const [name, setName] = useState(values.name ?? '');
  const { toggleFieldFocus, isFocused } = useFieldFocus();
  const versionDetails = values.version_details;
  // Memoized (not `?? []` inline): a fresh `[]` reference every render would
  // defeat `onChangeVariable`'s own memoization below (react-hooks/exhaustive-deps).
  const variables = useMemo(() => versionDetails?.variables ?? [], [versionDetails?.variables]);

  // Sync local state when the caller's value changes externally (e.g. on form reset/discard).
  useEffect(() => {
    if ((values.name ?? '') !== name) {
      setName(values.name ?? '');
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- resync only when the caller's own value changes, matching the baseline's identical dependency list.
  }, [values.name]);

  const onChangeVariable = useCallback(
    (label: string, newValue: string) => {
      onFieldChange(
        'version_details.variables',
        variables.map((variable: AgentVariable) => (variable.name === label ? { ...variable, value: newValue } : variable)),
      );
    },
    [onFieldChange, variables],
  );

  const onChangeName = useCallback(
    (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      setName(event.target.value);
      onFieldChange('name', event.target.value);
    },
    [onFieldChange],
  );

  const onNameFocus = useCallback(() => toggleFieldFocus(PROMPT_PAYLOAD_KEY.name), [toggleFieldFocus]);

  const onNameBlur = useCallback(() => {
    const trimmedName = name.trim();
    setName(trimmedName);
    onFieldChange('name', trimmedName);
    toggleFieldFocus(null);
  }, [name, onFieldChange, toggleFieldFocus]);

  const onDescriptionChange = useCallback(
    (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      onFieldChange('description', event.target.value);
    },
    [onFieldChange],
  );

  const onDescriptionFocus = useCallback(() => toggleFieldFocus(PROMPT_PAYLOAD_KEY.description), [toggleFieldFocus]);

  const onDescriptionBlur = useCallback(
    (_event: FocusEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      toggleFieldFocus(null);
    },
    [toggleFieldFocus],
  );

  // Writes the text, then re-derives the variable rows from it — see
  // `deriveVariablesFromInstructions` above for the baseline this restores.
  // Both writes go through the same `onFieldChange`, and every mount routes
  // both paths (`pages/agents/CreateApplication.tsx`,
  // `pages/pipelines/CreatePipeline.tsx`,
  // `pages/agents/lib/useEditApplicationVersionFields.ts`, and
  // `lib/useAgentEditorCreate.ts`'s generic `setFieldValueAtPath`), so the
  // rows appear on all four without any of them changing.
  const onInstructionsChange = useCallback(
    (value: string) => {
      onFieldChange('version_details.instructions', value);
      const derived = deriveVariablesFromInstructions(value, variables);
      if (!sameVariables(derived, variables)) {
        onFieldChange('version_details.variables', derived);
      }
    },
    [onFieldChange, variables],
  );

  const onWelcomeMessageChange = useCallback(
    (value: string) => onFieldChange('version_details.welcome_message', value),
    [onFieldChange],
  );

  const onStepLimitChange = useCallback(
    (value: number | undefined) => onFieldChange('version_details.meta.step_limit', value),
    [onFieldChange],
  );

  const onOutputContractChange = useCallback(
    (next: OutputContract | null) => onFieldChange('version_details.meta.output_contract', next),
    [onFieldChange],
  );

  const onConversationStartersChange = useCallback(
    (next: readonly string[]) => onFieldChange('version_details.conversation_starters', next),
    [onFieldChange],
  );

  return {
    name,
    description: values.description ?? '',
    nameAtMax: name.length === MAX_NAME_LENGTH,
    nameFocused: isFocused(PROMPT_PAYLOAD_KEY.name),
    descriptionFocused: isFocused(PROMPT_PAYLOAD_KEY.description),
    variables,
    onChangeName,
    onNameFocus,
    onNameBlur,
    onDescriptionChange,
    onDescriptionFocus,
    onDescriptionBlur,
    onChangeVariable,
    onInstructionsChange,
    onWelcomeMessageChange,
    onStepLimitChange,
    outputContract: storedOutputContract(values),
    onOutputContractChange,
    onConversationStartersChange,
  };
}
