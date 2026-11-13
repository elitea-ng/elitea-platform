import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import Box from '@mui/material/Box';

import { ExpandedViewerModal } from '@/shared/ui/ExpandedViewerModal';
import { combineSx } from '@/shared/ui/lib/combineSx';
import { capitalizeFirstChar } from '@/shared/lib/string';

import { AI_ASSISTANT_LANGUAGE_OPTIONS, detectContentType } from '../lib/aiAssistantLanguage';
import type { FStringAutocompleteOption } from '../lib/fStringAutocomplete';
import { useAIContentGenerationStreaming } from '../model/useAIContentGenerationStreaming';
import { useAiAssistantLanguageLinter } from '../model/useAiAssistantLanguageLinter';
import { useAiAssistantStreamSync } from '../model/useAiAssistantStreamSync';
import type { AiAssistantLlmSettings } from '../api/aiAssistantPredict';
import { AIAssistantModalSingleView } from './AIAssistantModalSingleView';
import { AIAssistantModalSplitView } from './AIAssistantModalSplitView';
import { AIPromptInput } from './AIPromptInput';
import type { PromptInputHandle } from './AIPromptInput';
import { contentBackgroundSx, contentWrapperSx, errorBannerSx } from './aiAssistantModal.styles';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/ai-assistant/
 * ui/AIAssistantModal.jsx` (baseline, 582 lines) — unit A2a. Split into
 * this file (orchestration + single view) + `./AIAssistantModalSplitView.tsx`
 * (split-view panels) + `./aiAssistantModal.styles.ts` (style objects) +
 * `../model/useAiAssistantStreamSync.ts` (2 of the baseline's 4 `useEffect`s)
 * to satisfy the §3.5 400-line file-length and 3-useEffects-per-component
 * budgets — no behaviour change from splitting alone.
 *
 * DEVIATIONS FROM BASELINE, all forced by real, verified constraints (see
 * each dependency's own doc comment for the full rationale — summarised
 * here):
 *  - Props are grouped into `fieldBinding`/`fStringAutocomplete`/
 *    `pipelineContext` objects (§3.5 12-prop budget: the baseline's flat
 *    prop list is 16).
 *  - `FlowEditorContext` (`@/[fsd]/app/providers`) is NOT read internally.
 *    No such context exists in this port yet (`flow-editor` is a separate,
 *    not-yet-built A2 sub-unit), and reaching into `app/` from `features/`
 *    is forbidden either way (R-L1). `pipelineContext.stateVariablesInfo`/
 *    `availableNodesInfo` are explicit props instead — the caller (a future
 *    node-editor sub-unit, A2e-h) computes them via the already-landed
 *    `formatStateVariablesForPrompt`/`formatAvailableNodesForPrompt`
 *    (`../lib/helpers/state.helpers.ts`) and passes the formatted strings
 *    down, same "caller passes it down" pattern `features/mcps/model/
 *    useMcpAuthModal.ts` established for `projectId`.
 *  - `useToast()` calls are replaced with the `hasError`/`errorMessage`
 *    fields `useAIContentGenerationStreaming` already exposes for this
 *    exact reason — rendered inline via `ExpandedViewerModal`'s modal
 *    surface rather than a toast (see that hook's own doc comment,
 *    deviation 3).
 *  - `onKeyDown` is dropped — see `./AIAssistantCodeMirrorInput.tsx`'s doc
 *    comment.
 *  - `contentBackgroundSx` (a prop the baseline passed straight through to
 *    its own `ExpandedViewerModal`) is applied to this component's OWN
 *    `content` node instead — this app's ported `ExpandedViewerModal`
 *    (unit S1-H) exposes no such passthrough for `BaseModal`'s content
 *    slot (its own doc comment records this as a deliberate trim); wrapping
 *    the content in a `Box` with the same sx reproduces the identical
 *    rendered background.
 */
/**
 * Not exported: nothing outside this file imports it by name yet (every
 * current caller goes through the structurally-compatible `AiAssistantFieldBinding.
 * onChange`/`onInput` shape instead) — `knip --max-issues 0` (R-D1) flags an
 * exported symbol with zero external importers as dead. Re-export the
 * moment a real external caller needs to name this type explicitly, same
 * "add back when needed" precedent `features/credentials/api/
 * useConfigurations.ts`'s own scope note documents for the identical class
 * of not-yet-needed export.
 */
interface AiAssistantChangeEvent {
  readonly preventDefault: () => void;
  readonly target: { readonly value: string; readonly name?: string; readonly id?: string };
}

export interface AiAssistantFieldBinding {
  readonly name?: string;
  readonly id?: string;
  readonly hasOnChangeCallback?: boolean;
  readonly onChange?: (event: AiAssistantChangeEvent) => void;
  readonly onInput?: (event: AiAssistantChangeEvent) => void;
}

export interface AiAssistantFStringAutocomplete {
  readonly enabled?: boolean;
  readonly stateVariableOptions?: readonly FStringAutocompleteOption[];
}

export interface AiAssistantPipelineContext {
  readonly stateVariablesInfo?: string;
  readonly availableNodesInfo?: string;
}

export interface AIAssistantModalProps {
  readonly open: boolean;
  readonly onClose?: () => void;
  readonly value?: string;
  readonly title?: string;
  readonly fieldName?: string;
  readonly specifiedLanguage?: string;
  readonly disabled?: boolean;
  readonly projectId?: string | number;
  readonly modelConfig?: AiAssistantLlmSettings | null;
  readonly fieldBinding?: AiAssistantFieldBinding;
  readonly fStringAutocomplete?: AiAssistantFStringAutocomplete;
  readonly pipelineContext?: AiAssistantPipelineContext;
  /** Editor only: no prompt footer. Set where the AI assistant is not served, so the expand view still opens (#2007). */
  readonly withoutAssistant?: boolean;
}

function buildChangeEvent(value: string, name: string | undefined, id: string | undefined): AiAssistantChangeEvent {
  return { preventDefault: () => {}, target: { value, ...(name !== undefined ? { name } : {}), ...(id !== undefined ? { id } : {}) } };
}

/** `hasOnChangeCallback ? onChange : onInput` routing, shared by `handleBlur`/`handleApply` — extracted so neither inlines the branch twice, keeping the component's own cyclomatic complexity under the §3.5 budget (12). */
function dispatchFieldChange(fieldBinding: AiAssistantFieldBinding | undefined, value: string): void {
  const event = buildChangeEvent(value, fieldBinding?.name, fieldBinding?.id);
  if (fieldBinding?.hasOnChangeCallback) {
    fieldBinding.onChange?.(event);
  } else {
    fieldBinding?.onInput?.(event);
  }
}

const isErrorContent = (content: string): boolean => Boolean(content.trim()) && content.trimStart().startsWith('Error');

interface ResolvedAiAssistantModalOptions {
  readonly enableFStringAutocomplete: boolean;
  readonly stateVariableOptions: readonly FStringAutocompleteOption[];
  readonly stateVariablesInfo: string;
  readonly availableNodesInfo: string;
}

/** Collapses the `?.`/`??` default-resolution for the grouped option objects into one call — same complexity-budget rationale as {@link dispatchFieldChange}. */
function resolveAiAssistantModalOptions(
  fStringAutocomplete: AiAssistantFStringAutocomplete | undefined,
  pipelineContext: AiAssistantPipelineContext | undefined,
): ResolvedAiAssistantModalOptions {
  return {
    enableFStringAutocomplete: fStringAutocomplete?.enabled ?? false,
    stateVariableOptions: fStringAutocomplete?.stateVariableOptions ?? [],
    stateVariablesInfo: pipelineContext?.stateVariablesInfo ?? '',
    availableNodesInfo: pipelineContext?.availableNodesInfo ?? '',
  };
}

export function AIAssistantModal(props: AIAssistantModalProps): ReactNode {
  const {
    open,
    onClose,
    value = '',
    title,
    fieldName = '',
    specifiedLanguage,
    disabled,
    projectId,
    modelConfig,
    fieldBinding,
    fStringAutocomplete,
    pipelineContext,
    withoutAssistant = false,
  } = props;

  const { enableFStringAutocomplete, stateVariableOptions, stateVariablesInfo, availableNodesInfo } = resolveAiAssistantModalOptions(
    fStringAutocomplete,
    pipelineContext,
  );

  const [currentValue, setCurrentValue] = useState(value);
  const [improvedContent, setImprovedContent] = useState('');
  const [showSplitView, setShowSplitView] = useState(false);
  const promptInputRef = useRef<PromptInputHandle | null>(null);
  const prevOpenRef = useRef(false);

  const { generateContent, cancel, isGenerating, streamedContent, hasError, errorMessage, resetContent } =
    useAIContentGenerationStreaming({
      projectId,
      modelConfig,
      fieldName,
      stateVariablesInfo,
      availableNodesInfo,
    });
  const { extensions, onChangeLanguage, language } = useAiAssistantLanguageLinter(specifiedLanguage, null, isGenerating);

  useEffect(() => {
    setCurrentValue(value);
  }, [value]);

  // Autofocus prompt input when the modal just opened.
  useEffect(() => {
    if (open && !prevOpenRef.current) {
      requestAnimationFrame(() => {
        promptInputRef.current?.focus();
      });
    }
    prevOpenRef.current = open;
  }, [open]);

  const handleBlur = useCallback(
    (contentOverride: string) => {
      dispatchFieldChange(fieldBinding, contentOverride || currentValue);
    },
    [currentValue, fieldBinding],
  );

  const updateLanguageIfChanged = useCallback(
    (content: string) => {
      const detectedLanguage = detectContentType(content);
      if (detectedLanguage !== language) {
        onChangeLanguage(detectedLanguage);
      }
    },
    [language, onChangeLanguage],
  );

  useAiAssistantStreamSync({
    streamedContent,
    isGenerating,
    showSplitView,
    hasError,
    setCurrentValue,
    setImprovedContent,
    handleBlur,
    updateLanguageIfChanged,
    clearPrompt: () => promptInputRef.current?.clear(),
  });

  const onClickClose = useCallback(() => {
    handleBlur(currentValue);
    onClose?.();
  }, [currentValue, handleBlur, onClose]);

  const handleAIGenerate = useCallback(
    async (prompt: string) => {
      resetContent();
      setImprovedContent('');

      const isCurrentContentError = isErrorContent(currentValue);
      const isImprovedContentError = isErrorContent(improvedContent);

      if (showSplitView && (hasError || isImprovedContentError)) {
        await generateContent(prompt, currentValue);
      } else if (isCurrentContentError) {
        setCurrentValue('');
        setShowSplitView(false);
        await generateContent(prompt, '');
      } else if (currentValue.trim()) {
        setShowSplitView(true);
        await generateContent(prompt, currentValue);
      } else {
        setShowSplitView(false);
        await generateContent(prompt, '');
      }
    },
    [currentValue, generateContent, hasError, improvedContent, resetContent, showSplitView],
  );

  const handleApply = useCallback(() => {
    setCurrentValue(improvedContent);
    dispatchFieldChange(fieldBinding, improvedContent);
    updateLanguageIfChanged(improvedContent);
    setShowSplitView(false);
    setImprovedContent('');
    resetContent();
    if (!hasError) promptInputRef.current?.clear();
  }, [fieldBinding, hasError, improvedContent, resetContent, updateLanguageIfChanged]);

  const handleCloseSplitView = useCallback(() => {
    setShowSplitView(false);
    setImprovedContent('');
    resetContent();
  }, [resetContent]);

  const languageOptions = useMemo(
    () => AI_ASSISTANT_LANGUAGE_OPTIONS.map((option) => ({ label: option.label, value: option.value })),
    [],
  );

  return (
    <ExpandedViewerModal
      open={open}
      onClose={onClickClose}
      title={capitalizeFirstChar(title ?? '')}
      language={{ value: language, options: languageOptions, onChange: onChangeLanguage, disabled: isGenerating }}
      footer={
        withoutAssistant ? undefined : (
          <AIPromptInput
            disabled={disabled || isGenerating}
            onGenerate={handleAIGenerate}
            onStop={cancel}
            isLoading={isGenerating}
            promptValueRef={promptInputRef}
          />
        )
      }
      content={
        <Box sx={combineSx(contentWrapperSx, contentBackgroundSx(showSplitView))}>
          {errorMessage && <Box sx={errorBannerSx}>{errorMessage}</Box>}
          {showSplitView ? (
            <AIAssistantModalSplitView
              isGenerating={isGenerating}
              currentValue={currentValue}
              improvedContent={improvedContent}
              extensions={extensions}
              enableFStringAutocomplete={enableFStringAutocomplete}
              stateVariableOptions={stateVariableOptions}
              onCurrentChange={setCurrentValue}
              onImprovedChange={setImprovedContent}
              onApply={handleApply}
              onCloseSplitView={handleCloseSplitView}
            />
          ) : (
            <AIAssistantModalSingleView
              readOnly={Boolean(disabled) || isGenerating}
              value={currentValue}
              extensions={extensions}
              notifyChange={setCurrentValue}
              onBlur={handleBlur}
              enableFStringAutocomplete={enableFStringAutocomplete}
              stateVariableOptions={stateVariableOptions}
              isGenerating={isGenerating}
            />
          )}
        </Box>
      }
    />
  );
}
