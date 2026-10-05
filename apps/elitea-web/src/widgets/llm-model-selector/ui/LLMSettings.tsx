import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { Box } from '@mui/material';

import {
  DEFAULT_MAX_TOKENS,
  DEFAULT_MAX_TOKENS_CUSTOM,
  DEFAULT_REASONING_EFFORT,
  DEFAULT_STEPS_LIMIT,
  DEFAULT_TEMPERATURE,
} from '@/shared/lib/constants';
import { PROMPT_PAYLOAD_KEY } from '@/shared/lib/prompt-payload';
import { parseValueToIntNumber } from '@/shared/lib/number';
import { isNullOrUndefined } from '@/shared/lib/object';
import { VALIDATION_RULE, getMaxTokensHelperText, validateMaxTokens } from '@/widgets/llm-model-selector/lib/validation';
import { CapabilitySection } from './settings/CapabilitySection';
import { CreativitySlider } from './settings/CreativitySlider';
import { MaxTokensSection } from './settings/MaxTokensSection';
import { ReasoningSlider } from './settings/ReasoningSlider';
import { StepsLimitInput } from './settings/StepsLimitInput';

interface LLMSettingsProps {
  llmSettings?: Record<string, unknown>;
  model?: Record<string, unknown> | undefined;
  onChangeLLMSettings: (field: string) => (value: unknown) => void;
  showStepsLimit?: boolean;
}

/**
 * Below: pure helpers extracted from the `LLMSettings` component body to
 * keep it under the eslint(complexity) budget — each has its own, separate
 * complexity budget. Behavior is unchanged; only the branching/derivation
 * logic moved out of the component closure.
 */

/** `useState` initializer for `maxTokens`: default-token passthrough, else the current (or custom-default) value. */
function computeInitialMaxTokens(llmSettings: Record<string, unknown>): number {
  const current = llmSettings?.max_tokens as number;
  return current === DEFAULT_MAX_TOKENS ? DEFAULT_MAX_TOKENS : (current ?? DEFAULT_MAX_TOKENS_CUSTOM);
}

type MaxTokensBlurAction = { type: 'reset' } | { type: 'update'; value: number } | { type: 'none' };

/**
 * Decision logic for `onMaxTokensBlur`'s debounced check. `refocused` is a
 * live read of the "did the user tab back in within the debounce window"
 * ref — passed in rather than closed over, since it must reflect the ref's
 * value at the moment the debounce timer fires, not when it was scheduled.
 */
function resolveMaxTokensBlurAction(
  maxTokens: number | string,
  refocused: boolean,
  currentSettingsMaxTokens: number | undefined,
): MaxTokensBlurAction {
  if (!refocused && maxTokens !== DEFAULT_MAX_TOKENS && !maxTokens) {
    return { type: 'reset' };
  }
  if (maxTokens !== currentSettingsMaxTokens) {
    return {
      type: 'update',
      value: maxTokens === DEFAULT_MAX_TOKENS ? DEFAULT_MAX_TOKENS : parseInt(String(maxTokens), 10),
    };
  }
  return { type: 'none' };
}

/** Normalizes the max-tokens field's raw `onChange` payload (a DOM event, string, or number) into a parsed number. */
function parseMaxTokensInput(value: number | string | Event): number | '' {
  if ((value as Event)?.preventDefault) (value as Event).preventDefault();
  const eventTarget = (value as Event)?.target as { value?: string } | undefined;
  const raw = eventTarget?.value ?? (value as number | string);
  return parseValueToIntNumber(String(raw));
}

/** `[field, value]` pairs for any setting that's still unset and needs its default applied. */
function computeMissingDefaults(
  llmSettings: Record<string, unknown>,
  model: Record<string, unknown>,
  showStepsLimit: boolean,
): Array<[string, unknown]> {
  const updates: Array<[string, unknown]> = [];
  if (isNullOrUndefined(llmSettings?.temperature)) {
    updates.push([PROMPT_PAYLOAD_KEY.temperature, DEFAULT_TEMPERATURE]);
  }
  if ((llmSettings?.max_tokens as number | undefined) === undefined) {
    updates.push([PROMPT_PAYLOAD_KEY.maxTokens, DEFAULT_MAX_TOKENS]);
  }
  if ((model?.supports_reasoning as boolean | undefined) && isNullOrUndefined(llmSettings?.reasoning_effort)) {
    updates.push([PROMPT_PAYLOAD_KEY.reasoningEffort, DEFAULT_REASONING_EFFORT]);
  }
  if (showStepsLimit && isNullOrUndefined(llmSettings?.steps_limit)) {
    updates.push([PROMPT_PAYLOAD_KEY.stepsLimit, DEFAULT_STEPS_LIMIT]);
  }
  return updates;
}

/** Whether the model's capabilities call for the reasoning-effort slider instead of the temperature slider. */
function modelSupportsReasoning(model: Record<string, unknown>): boolean {
  return 'supports_reasoning' in model && Boolean(model.supports_reasoning);
}

interface DerivedSettingsValues {
  reasoningEffort: string;
  temperature: number;
  stepsLimit: number;
  supportsVision: boolean;
  supportsReasoning: boolean;
}

/**
 * Display values for the settings controls below — each is `llmSettings`/
 * `model`'s raw field, or its default. `llmSettings`/`model` are always
 * objects here (never `undefined`) — both params are non-optional, so
 * plain member access is used rather than `?.` (also keeps this under the
 * eslint(complexity) budget, which counts optional-chaining links).
 */
function deriveDisplaySettings(
  llmSettings: Record<string, unknown>,
  model: Record<string, unknown>,
): DerivedSettingsValues {
  return {
    reasoningEffort: (llmSettings.reasoning_effort as string) ?? DEFAULT_REASONING_EFFORT,
    temperature: (llmSettings.temperature as number) ?? DEFAULT_TEMPERATURE,
    stepsLimit: (llmSettings.steps_limit as number) ?? DEFAULT_STEPS_LIMIT,
    supportsVision: (model.supports_vision as boolean) ?? false,
    supportsReasoning: (model.supports_reasoning as boolean) ?? false,
  };
}

/**
 * LLM settings form panel.
 * Ported from `[fsd]/widgets/llm-model-selector/ui/LLMSettings.jsx`.
 */
export const LLMSettings = memo(
  ({
    llmSettings = {},
    model = {},
    onChangeLLMSettings,
    showStepsLimit = false,
  }: LLMSettingsProps) => {
    const focusOnMaxTokens = useRef(false);

    const [maxTokens, setMaxTokens] = useState(computeInitialMaxTokens(llmSettings));

    const onMaxTokensBlur = useCallback(() => {
      focusOnMaxTokens.current = false;
      setTimeout(() => {
        const action = resolveMaxTokensBlurAction(
          maxTokens,
          focusOnMaxTokens.current,
          llmSettings?.max_tokens as number,
        );
        if (action.type === 'reset') {
          onChangeLLMSettings(PROMPT_PAYLOAD_KEY.maxTokens)(DEFAULT_MAX_TOKENS);
          setMaxTokens(DEFAULT_MAX_TOKENS);
        } else if (action.type === 'update') {
          onChangeLLMSettings(PROMPT_PAYLOAD_KEY.maxTokens)(action.value);
        }
      }, 50);
    }, [llmSettings?.max_tokens, maxTokens, onChangeLLMSettings]);

    const onMaxTokensFocus = useCallback(() => {
      focusOnMaxTokens.current = true;
    }, []);

    const onInputMaxTokens = useCallback(
      (value: number | string | Event) => {
        if (value === DEFAULT_MAX_TOKENS) {
          onChangeLLMSettings(PROMPT_PAYLOAD_KEY.maxTokens)(DEFAULT_MAX_TOKENS);
          setMaxTokens(DEFAULT_MAX_TOKENS);
          return;
        }
        const parsed = parseMaxTokensInput(value);
        onChangeLLMSettings(PROMPT_PAYLOAD_KEY.maxTokens)(parsed);
        setMaxTokens(parsed as number);
      },
      [onChangeLLMSettings],
    );

    useEffect(() => {
      if ((llmSettings?.max_tokens as number) !== maxTokens) {
        setMaxTokens(llmSettings?.max_tokens as number);
      }
    }, [llmSettings?.max_tokens, maxTokens]);

    const initializeDefaults = useCallback(() => {
      const updates = computeMissingDefaults(llmSettings, model, showStepsLimit);
      updates.forEach(([key, value]) => onChangeLLMSettings(key)(value));
    }, [llmSettings, model, showStepsLimit, onChangeLLMSettings]);

    useEffect(() => {
      initializeDefaults();
    }, [initializeDefaults]);

    const { maxTokensError, maxTokensHelperText } = useMemo(() => {
      const result = validateMaxTokens(maxTokens, model);
      return {
        maxTokensError: result !== VALIDATION_RULE.VALID,
        maxTokensHelperText: getMaxTokensHelperText(result, model),
      };
    }, [maxTokens, model]);

    const showReasoningSlider = modelSupportsReasoning(model);
    const derived = deriveDisplaySettings(llmSettings, model);

    return (
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2, px: 4 }}>
        {showReasoningSlider ? (
          <ReasoningSlider
            value={derived.reasoningEffort}
            onChange={onChangeLLMSettings(PROMPT_PAYLOAD_KEY.reasoningEffort)}
            disabled={false}
          />
        ) : (
          <CreativitySlider
            temperature={derived.temperature}
            onChange={onChangeLLMSettings(PROMPT_PAYLOAD_KEY.temperature)}
          />
        )}
        <MaxTokensSection
          value={maxTokens}
          onChange={onInputMaxTokens}
          onBlur={onMaxTokensBlur}
          onFocus={onMaxTokensFocus}
          maxOutputTokens={model?.max_output_tokens as number | undefined}
          error={maxTokensError}
          helperText={maxTokensHelperText}
        />
        {showStepsLimit && (
          <StepsLimitInput
            value={derived.stepsLimit}
            onChange={onChangeLLMSettings(PROMPT_PAYLOAD_KEY.stepsLimit)}
          />
        )}
        <CapabilitySection
          supportsVision={derived.supportsVision}
          supportsReasoning={derived.supportsReasoning}
        />
      </Box>
    );
  },
);

LLMSettings.displayName = 'LLMSettings';
