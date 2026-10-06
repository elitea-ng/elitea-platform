import type { FocusEvent, KeyboardEvent, MouseEvent, ReactNode } from 'react';
import { useCallback, useMemo } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { hasBackendCapability } from '@/shared/config';
import { t } from '@/shared/i18n';
import { HeadingChip } from '@/shared/ui/HeadingChip';
import { SingleSelect, type SingleSelectOption } from '@/shared/ui/SingleSelect';
import { StyledInputEnhancer } from '@/shared/ui/StyledInputEnhancer';
import { capitalizeFirstChar } from '@/shared/lib/string';

import { AIAssistantInput } from '../AIAssistantInput';
import type { AiAssistantLlmSettings } from '../../api/aiAssistantPredict';
import { FStringAutocompletePopper } from '../FStringAutocompletePopper';
import { useFStringInputAutocomplete } from '../../model/useFStringInputAutocomplete';
import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import { useInputOptions } from '../../lib/flow-editor/hooks/useInputOptions';

/** Minimal shape both `AIAssistantInput`'s `fieldBinding.onInput` and a plain `<input>` change event satisfy. */
interface SimpleLLMInputChangeEvent {
  readonly preventDefault: () => void;
  readonly target: { readonly value: string };
}

export interface SimpleLLMInputMappingValue {
  readonly type?: string;
  readonly value: unknown;
}

interface NodeFieldInputProps {
  readonly shouldEnableAIAssistant: boolean;
  readonly variable: string;
  readonly value: string;
  readonly disabled?: boolean | undefined;
  readonly onInput: (event: SimpleLLMInputChangeEvent) => void;
  readonly variableName: string;
  readonly enableFStringAutocomplete: boolean;
  readonly stateVariableOptions: readonly SingleSelectOption[];
  readonly modelConfig?: AiAssistantLlmSettings | null;
  /** Selected Code language. Its presence also enables multiline source input. */
  readonly language?: string;
}

/**
 * `apps/elitea-ui/…/settings/SimpleLLMInputItem.jsx:14-108`'s co-located
 * `NodeFieldInput`. Renders either `AIAssistantInput` (already ported,
 * unit A2a) or `shared/ui/StyledInputEnhancer` (unit S1-F), matching the
 * baseline's `shouldEnableAIAssistant` branch.
 *
 * **FORMERLY A CONFIRMED REGRESSION (finding #2 of this cluster,
 * A2-settings-panels), now fixed one file over:** `../AIAssistantInput.tsx`'s
 * `buildInputBaseProps` used to forward only `value`/`label`/`disabled`/
 * `name`/`id` onto `InputBase`, with no `onChange` at all, so typing
 * directly into an AI-Assistant-enabled field's visible (collapsed) input
 * was silently swallowed — editing only worked through the AI Assistant
 * modal. `AIAssistantInput.tsx` now wires a real `onChange` (its own
 * `dispatchInlineFieldChange`, fixed there as that file's own finding #1) that
 * routes to `fieldBinding.onInput` — the exact same handler this file already
 * threads in below (`fieldBinding={{ name: 'value', id: ..., onInput }}`,
 * the identical function backing the plain branch's `onChange={handleChange}`
 * further down). Nothing in THIS file needed to change for the fix to take
 * effect — recorded here (and covered by a `SimpleLLMInputItem.test.tsx`
 * integration test) only because this doc comment previously — incorrectly —
 * described the gap as permanent and unfixable from either file.
 *
 * Separately, disclosed and NOT a regression — forced by the already-landed
 * `AIAssistantInput`'s reduced prop surface (`value`/`label`/`fieldName`/
 * `language`/`disabled`/`projectId`/`modelConfig`/`fieldBinding`/
 * `fStringAutocomplete`/`pipelineContext` only, vs. the baseline's ~20 flat
 * props): the F-string autocomplete popper + live cursor tracking this file
 * wires via `useFStringInputAutocomplete` only applies to the plain
 * `StyledInputEnhancer` branch; the AI-Assistant branch forwards
 * `fStringAutocomplete` straight to `AIAssistantInput`, which threads it
 * into its own modal (already-landed behaviour, not duplicated here).
 *
 * Also dropped: CodeMirror `language` syntax highlighting on the plain
 * branch -- `StyledInputEnhancer`'s own doc comment already discloses this
 * ("none of which exist in `shared/ui` yet").
 */
function NodeFieldInput(props: NodeFieldInputProps): ReactNode {
  const { shouldEnableAIAssistant, variable, value, disabled = false, onInput, variableName, enableFStringAutocomplete, stateVariableOptions, modelConfig = null, language } = props;

  const {
    closeAutocomplete,
    containerRef,
    filteredOptions: filteredStateVariableOptions,
    handleAutocompleteKeyDown,
    handleChange,
    handleCursorChange,
    handleSuggestionSelect,
    highlightedOptionIndex,
    inputRef,
  } = useFStringInputAutocomplete({
    resolvedValue: value,
    onInput,
    enabled: enableFStringAutocomplete && !disabled,
    options: stateVariableOptions.map(option => ({ label: option.label, value: option.value })),
  });

  const isChatHistory = variable === 'chat_history';
  const isMultiline = isChatHistory || language !== undefined;

  /**
   * `handleCursorChange` (from `useFStringInputAutocomplete`) reads
   * `event.target.value`/`event.target.selectionStart` off whatever
   * element the DOM event actually fired on -- the real `<input>` or `<textarea>`
   * at runtime, even though React's `MouseEvent<HTMLDivElement>`/
   * `FocusEvent<HTMLDivElement>`/`KeyboardEvent<HTMLDivElement>` handler
   * types (attached by `StyledInputEnhancer`'s `TextFieldProps`) declare
   * `target: EventTarget` generically. A local adapter (rather than passing
   * the hook's function reference directly, which `tsc` rejects -- the
   * declared `target: EventTarget` does not structurally satisfy the
   * hook's `{value?, selectionStart?}` shape even though both fields are
   * optional) preserves the baseline's identical runtime behaviour.
   */
  const toCursorTarget = (target: EventTarget | null): { value?: string; selectionStart?: number | null } =>
    target as unknown as { value?: string; selectionStart?: number | null };
  const onFieldClick = useCallback((event: MouseEvent<HTMLDivElement>) => handleCursorChange({ target: toCursorTarget(event.target) }), [handleCursorChange]);
  const onFieldFocus = useCallback((event: FocusEvent<HTMLInputElement | HTMLTextAreaElement>) => handleCursorChange({ target: toCursorTarget(event.target) }), [handleCursorChange]);
  const onFieldKeyUp = useCallback((event: KeyboardEvent<HTMLDivElement>) => handleCursorChange({ target: toCursorTarget(event.target) }), [handleCursorChange]);

  return (
    <Box ref={containerRef}>
      {shouldEnableAIAssistant ? (
        <AIAssistantInput
          value={value}
          label={t('pipelines.simpleLLMInputItem.value', 'Value')}
          fieldName={variableName}
          disabled={disabled}
          modelConfig={modelConfig}
          fieldBinding={{ name: 'value', id: `${variable}-value`, onInput }}
          fStringAutocomplete={{ enabled: enableFStringAutocomplete, stateVariableOptions }}
          {...(language !== undefined ? { language } : {})}
        />
      ) : (
        <StyledInputEnhancer
          label={t('pipelines.simpleLLMInputItem.value', 'Value')}
          value={value}
          disabled={disabled}
          name="value"
          id={`${variable}-value`}
          {...(isMultiline ? { expand: { minRows: 3, maxRows: 8 } } : {})}
          actions={{ enabled: true, showCopy: true, showFullScreen: true, showExpand: isMultiline }}
          onChange={handleChange}
          onBlur={closeAutocomplete}
          onClick={onFieldClick}
          onFocus={onFieldFocus}
          onKeyDown={handleAutocompleteKeyDown}
          onKeyUp={onFieldKeyUp}
          inputRef={inputRef}
        />
      )}
      <FStringAutocompletePopper
        open={filteredStateVariableOptions.length > 0 && autocompleteIsOpen(shouldEnableAIAssistant, filteredStateVariableOptions.length)}
        anchorEl={containerRef.current}
        options={filteredStateVariableOptions}
        highlightedIndex={highlightedOptionIndex}
        onSelect={handleSuggestionSelect}
      />
    </Box>
  );
}

/** `filteredOptions.length > 0 && autocompleteState.isOpen` in the baseline -- collapsed here into a helper so `NodeFieldInput`'s JSX does not read `autocompleteState` directly (that hook value is already folded into `filteredOptions` staying empty when closed; kept as a named helper purely for readability parity with the baseline condition). */
function autocompleteIsOpen(shouldEnableAIAssistant: boolean, filteredCount: number): boolean {
  return !shouldEnableAIAssistant && filteredCount > 0;
}

/**
 * Baseline: `apps/elitea-ui/…/settings/SimpleLLMInputItem.jsx:126-127` —
 * `(type === 'fstring' || type === 'fixed') && variableName.toLowerCase() === 'code'
 * ? 'python' : undefined`. The editor now uses the selected Code language.
 * Omitted language retains Python instead of falling back to `AIAssistantInput`'s own
 * `detectContentType` heuristic (`../../lib/aiAssistantLanguage.ts`), which can
 * easily guess "text" for a short/empty code snippet. This was dropped during
 * the initial port (a real regression, not a disclosed trim) — restored here.
 * Extracted to a standalone function (rather than inlined in
 * `SimpleLLMInputItem`'s body) purely to keep that component's own
 * cyclomatic complexity under the §3.5 budget (12) — same technique
 * {@link autocompleteIsOpen} above uses.
 *
 * Restoring this does NOT by itself turn on real Python CodeMirror syntax
 * highlighting: `../../model/useAiAssistantLanguageLinter.ts`'s
 * `getExtensionsForLanguage` still routes `'python'` to the same no-highlight
 * bucket as `'text'`, because `@codemirror/lang-python` is not an installed
 * dependency (see that file's header doc comment). Adding that package is a
 * `package.json` edit — a shared, cross-cutting file this sub-unit (owns only
 * `src/features/pipelines/`) should not change unilaterally in a worktree
 * shared with sibling units. This selects the configured content type and retains the Python default.
 * It also preserves detected-language bookkeeping; real highlighting is
 * a follow-up that needs the dependency added elsewhere.
 */
function resolveCodeFieldLanguage(type: string, variableName: string, codeLanguage: string | undefined): string | undefined {
  return (type === 'fstring' || type === 'fixed') && variableName.toLowerCase() === 'code' ? (codeLanguage ?? 'python') : undefined;
}

export interface SimpleLLMInputItemProps {
  readonly variableName: string;
  readonly variable: string;
  readonly type: string;
  readonly value: unknown;
  readonly defaultValue: unknown;
  readonly onChangeMapping: (variable: string, value: SimpleLLMInputMappingValue) => void;
  readonly disabled?: boolean | undefined;
  readonly enableAIAssistant?: boolean;
  readonly modelConfig?: AiAssistantLlmSettings | null;
  readonly codeLanguage?: string | undefined;
}

const containerSx: SxProps<Theme> = {};
const fieldRowSx: SxProps<Theme> = { display: 'flex', gap: '0.75rem', alignItems: 'flex-start', minHeight: '3.79rem' };
const typeSelectWrapperSx: SxProps<Theme> = { width: '7.25rem', transform: 'translateY(0.89rem)' };
const selectSx: SxProps<Theme> = { marginBottom: '0rem' };

function valueWrapperSx(isStringType: boolean): SxProps<Theme> {
  return { flex: 1, transform: isStringType ? undefined : 'translateY(0.89rem)' };
}

const AI_ASSISTANT_VARIABLE_NAMES: ReadonlySet<string> = new Set(['system', 'task', 'code', 'printer', 'user_message']);

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * settings/SimpleLLMInputItem.jsx` (unit A2h). See `NodeFieldInput`'s doc
 * comment above for the AI-Assistant-branch redesign this file is built
 * around.
 */
export function SimpleLLMInputItem(props: SimpleLLMInputItemProps): ReactNode {
  const { variableName, variable, type, value, defaultValue, onChangeMapping, disabled = false, enableAIAssistant = false, modelConfig = null, codeLanguage } = props;

  const typeOptions = useMemo<SingleSelectOption[]>(
    () => FlowEditorConstants.agentTaskTypeOptions.map(option => ({ label: option.label, value: option.value })),
    [],
  );
  const stateVariableOptions = useInputOptions();

  const resolvedValue = typeof value !== 'string' ? JSON.stringify(value) : value;

  const onChange = useCallback(
    (field: 'type' | 'value', newValue: string) => {
      if (field === 'type') {
        const shouldPreserveValue = (type === 'fstring' && newValue === 'fixed') || (type === 'fixed' && newValue === 'fstring');
        onChangeMapping(variable, { type: newValue, value: shouldPreserveValue ? value : defaultValue });
      } else {
        onChangeMapping(variable, { type, value: newValue });
      }
    },
    [onChangeMapping, variable, type, value, defaultValue],
  );

  const onInput = useCallback(
    (event: SimpleLLMInputChangeEvent) => {
      event.preventDefault();
      if (variableName.toLowerCase() === 'chat_history' && type === 'fixed') {
        try {
          const parsedValue: unknown = JSON.parse(event.target.value);
          onChangeMapping(variable, { type, value: parsedValue });
        } catch {
          onChange('value', event.target.value);
        }
      } else {
        onChange('value', event.target.value);
      }
    },
    [onChange, onChangeMapping, type, variable, variableName],
  );

  // The capability is the first term because the assistant's only
  // action POSTs `predict_llm`, which no router mounts — see
  // `shared/config/backendCapabilities`. The field then renders as the plain
  // input, which is the same field without the assistant button.
  const shouldEnableAIAssistant = useMemo(
    () => hasBackendCapability('llmPredictStreaming') && enableAIAssistant && (type === 'fstring' || type === 'fixed') && AI_ASSISTANT_VARIABLE_NAMES.has(variableName),
    [enableAIAssistant, type, variableName],
  );

  const enableFStringAutocomplete = type === 'fstring' && FlowEditorConstants.FSTRING_AUTOCOMPLETE_VARIABLES.has(variableName);

  const isStringType = type === 'string' || type === 'fstring' || type === 'fixed';

  const codeFieldLanguage = resolveCodeFieldLanguage(type, variableName, codeLanguage);

  return (
    <Box sx={containerSx}>
      <HeadingChip label={capitalizeFirstChar(variableName.replaceAll('_', ' '))} />

      <Box sx={fieldRowSx}>
        {/*
         * `nopan nodrag` — React Flow's escape hatch, and it is load-bearing
         * for the two `SingleSelect`s below. The canvas's drag layer handles
         * the mouse-down on the node, so without it a click on either
         * dropdown FOCUSES the combobox and never opens its menu: measured on
         * the standalone stack, the accessibility tree showed the combobox
         * `[active]` with no `listbox` anywhere. Keyboard (focus + Enter)
         * still worked, which is why unit tests saw a working control and
         * only an e2e click caught it.
         *
         * MUI's `Select` is a `div[role="combobox"]` that opens from its own
         * click handler, which is why it breaks where a plain `<input>` does
         * not — an input still takes native focus. `hasSelector`
         * (@xyflow/system) walks UP from the event target, so the class sits
         * on these wrappers rather than on `SingleSelect`: that component is
         * shared and already at the §3.5 12-prop budget, so giving it a
         * `className` fails the gate.
         *
         * The sibling selects on this card carry the same escape hatch via
         * `PipelineMultiSelect` (`ui/select/InputSelect.tsx` and its two
         * siblings). They add `nowheel` as well; these do not, because their
         * menus render in a portal outside the canvas and wheel-to-zoom over
         * the closed control is the canvas behaviour a user expects.
         */}
        <Box
          className="nopan nodrag"
          sx={typeSelectWrapperSx}
        >
          <SingleSelect
            sx={selectSx}
            label={t('pipelines.simpleLLMInputItem.type', 'Type')}
            value={type}
            onChange={newValue => onChange('type', newValue)}
            options={typeOptions}
            disabled={disabled}
          />
        </Box>
        <Box
          className="nopan nodrag"
          sx={valueWrapperSx(isStringType)}
        >
          {isStringType ? (
            <NodeFieldInput
              shouldEnableAIAssistant={shouldEnableAIAssistant}
              variable={variable}
              value={resolvedValue}
              disabled={disabled}
              onInput={onInput}
              variableName={variableName}
              enableFStringAutocomplete={enableFStringAutocomplete}
              stateVariableOptions={stateVariableOptions}
              modelConfig={modelConfig}
              {...(codeFieldLanguage !== undefined ? { language: codeFieldLanguage } : {})}
            />
          ) : (
            <SingleSelect
              sx={selectSx}
              label={t('pipelines.simpleLLMInputItem.value', 'Value')}
              value={typeof value === 'string' ? value : ''}
              onChange={newValue => onChange('value', newValue)}
              options={stateVariableOptions}
              disabled={disabled}
            />
          )}
        </Box>
      </Box>
    </Box>
  );
}
