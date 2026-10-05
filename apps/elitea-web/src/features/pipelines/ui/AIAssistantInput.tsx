import type { ChangeEvent, ReactNode } from 'react';
import { memo, useCallback, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import SvgIcon from '@mui/material/SvgIcon';
import Tooltip from '@mui/material/Tooltip';
import type { SxProps, Theme } from '@mui/material/styles';

import { InputBase } from '@/shared/ui/InputBase';
import type { InputBaseProps } from '@/shared/ui/InputBase';
import { AiAssistantIcon } from '@/shared/ui/icons/ai-assistant-icon';
import { hasBackendCapability } from '@/shared/config';
import { t } from '@/shared/i18n';

import { AIAssistantModal } from './AIAssistantModal';
import type { AIAssistantModalProps, AiAssistantFieldBinding, AiAssistantFStringAutocomplete, AiAssistantPipelineContext } from './AIAssistantModal';
import { detectContentType } from '../lib/aiAssistantLanguage';
import type { AiAssistantLlmSettings } from '../api/aiAssistantPredict';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/ai-assistant/
 * ui/AIAssistantInput.jsx` (baseline, 65 lines) — unit A2a.
 *
 * DEVIATIONS FROM BASELINE, both forced by real, verified constraints:
 *
 *  1. Explicit, grouped props instead of `{...leftProps}` spread onto both
 *     `Input.InputBase` and `AIAssistantModal`. `fieldBinding`/
 *     `fStringAutocomplete`/`pipelineContext` are the SAME grouped shapes
 *     `AIAssistantModal.tsx` declares (its own doc comment covers the §3.5
 *     12-prop-budget rationale) — passed straight through here rather than
 *     re-destructured, so this component's own prop count stays low too.
 *     `onKeyDown` is dropped — see `./AIAssistantCodeMirrorInput.tsx`'s doc
 *     comment for why the underlying editor primitive cannot fire it.
 *
 *  2. `Input.InputBase`'s `fullScreenIcon`/`showFullScreenAction` props
 *     have no equivalent on this app's ported `shared/ui/InputBase` (unit
 *     S1-F) — see this file's own trigger-button rendering below, same
 *     rationale as the previous revision of this doc comment.
 *
 * FIX (confirmed adversarial-review finding #1, this file:60): the inline
 * (collapsed, non-modal) `InputBase` rendered below used to receive `value`
 * with no change handler at all — React resets a controlled `value` on
 * every re-render, so typing directly into the field was silently discarded
 * and the AI Assistant modal's own apply/close handlers (which DO call
 * `fieldBinding.onInput`/`onChange`, see `AIAssistantModal.tsx`'s
 * `dispatchFieldChange`) were the ONLY way to ever change the value.
 * `buildInputBaseProps` now forwards a real `onChange` that routes through
 * the same `hasOnChangeCallback ? onChange : onInput` logic via
 * `dispatchInlineFieldChange` below — duplicated locally rather than
 * imported because `AIAssistantModal.tsx`'s `dispatchFieldChange` is
 * deliberately unexported (that file's own "Not exported" doc comment) and
 * is outside this fix's file scope. This resolves the gap every downstream
 * caller's own doc comment discloses as "rooted in this file, not this
 * sub-unit's file to change" (`nodes/RouterNode.tsx`, `nodes/
 * StateModifierNode.tsx`, `nodes/PrinterNode.tsx`,
 * `settings/SimpleLLMInputItem.tsx`) — those callers need no further
 * change, but their doc comments describing this as a live, deferred gap
 * are now stale and should be trimmed by whoever next touches those files.
 */
export interface AIAssistantInputProps {
  readonly value: string;
  readonly label?: ReactNode;
  readonly fieldName?: string;
  readonly language?: string;
  readonly disabled?: boolean;
  readonly projectId?: string | number;
  readonly modelConfig?: AiAssistantLlmSettings | null;
  readonly fieldBinding?: AiAssistantFieldBinding;
  readonly fStringAutocomplete?: AiAssistantFStringAutocomplete;
  readonly pipelineContext?: AiAssistantPipelineContext;
}

const triggerButtonSx: SxProps<Theme> = (theme) => ({
  position: 'absolute',
  top: theme.spacing(-2.5),
  right: theme.spacing(1.5),
  zIndex: 1,
});

/**
 * Minimal duck-typed change-event shape `AiAssistantFieldBinding.onChange`/
 * `onInput` accept — structurally mirrors `AIAssistantModal.tsx`'s own
 * (non-exported) `AiAssistantChangeEvent`, reproduced here rather than
 * imported (see this file's module doc comment, "FIX" paragraph).
 */
interface InlineFieldChangeEvent {
  readonly preventDefault: () => void;
  readonly target: { readonly value: string; readonly name?: string; readonly id?: string };
}

/**
 * Routes the inline field's native `onChange` into `fieldBinding.onChange`/
 * `onInput` — the same `hasOnChangeCallback` branch `AIAssistantModal.tsx`'s
 * `dispatchFieldChange` uses for its apply/blur paths, duplicated locally
 * (see this file's module doc comment, "FIX" paragraph, for why it is not
 * imported instead).
 */
function dispatchInlineFieldChange(fieldBinding: AiAssistantFieldBinding | undefined, value: string): void {
  if (!fieldBinding) return;
  const event: InlineFieldChangeEvent = {
    preventDefault: () => {},
    target: {
      value,
      ...(fieldBinding.name !== undefined ? { name: fieldBinding.name } : {}),
      ...(fieldBinding.id !== undefined ? { id: fieldBinding.id } : {}),
    },
  };
  if (fieldBinding.hasOnChangeCallback) {
    fieldBinding.onChange?.(event);
  } else {
    fieldBinding.onInput?.(event);
  }
}

/** Collapses the `InputBase` prop-forwarding ternaries into one call — extracted to keep the component's own cyclomatic complexity under the §3.5 budget (12), same technique `../ui/AIAssistantModal.tsx`'s `resolveAiAssistantModalOptions` uses. */
function buildInputBaseProps(
  value: string,
  label: ReactNode,
  disabled: boolean | undefined,
  fieldBinding: AiAssistantFieldBinding | undefined,
): InputBaseProps {
  return {
    value,
    ...(label !== undefined ? { label } : {}),
    ...(disabled !== undefined ? { disabled } : {}),
    ...(fieldBinding?.name !== undefined ? { name: fieldBinding.name } : {}),
    ...(fieldBinding?.id !== undefined ? { id: fieldBinding.id } : {}),
    onChange: (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      dispatchInlineFieldChange(fieldBinding, event.target.value);
    },
  };
}

/** Same rationale as {@link buildInputBaseProps}, for the `AIAssistantModal` forwarding. */
function buildAiAssistantModalProps(
  required: Pick<AIAssistantModalProps, 'value' | 'title' | 'fieldName' | 'open' | 'onClose' | 'specifiedLanguage' | 'modelConfig'>,
  optional: {
    readonly disabled: boolean | undefined;
    readonly projectId: string | number | undefined;
    readonly fieldBinding: AiAssistantFieldBinding | undefined;
    readonly fStringAutocomplete: AiAssistantFStringAutocomplete | undefined;
    readonly pipelineContext: AiAssistantPipelineContext | undefined;
  },
): AIAssistantModalProps {
  return {
    ...required,
    ...(optional.disabled !== undefined ? { disabled: optional.disabled } : {}),
    ...(optional.projectId !== undefined ? { projectId: optional.projectId } : {}),
    ...(optional.fieldBinding !== undefined ? { fieldBinding: optional.fieldBinding } : {}),
    ...(optional.fStringAutocomplete !== undefined ? { fStringAutocomplete: optional.fStringAutocomplete } : {}),
    ...(optional.pipelineContext !== undefined ? { pipelineContext: optional.pipelineContext } : {}),
  };
}

export const AIAssistantInput = memo(function AIAssistantInput(props: AIAssistantInputProps): ReactNode {
  const {
    value = '',
    label,
    fieldName = '',
    language,
    disabled,
    projectId,
    modelConfig = null,
    fieldBinding,
    fStringAutocomplete,
    pipelineContext,
  } = props;

  const [showAIAssistantModal, setShowAIAssistantModal] = useState(false);

  const handleOpenAIAssistant = useCallback(() => setShowAIAssistantModal(true), []);
  const handleCloseAIAssistant = useCallback(() => setShowAIAssistantModal(false), []);

  const detectedLanguage = useMemo(() => language ?? detectContentType(value), [language, value]);

  const triggerLabel = t('pipelines.aiAssistant.input.openTrigger', 'AI Assistant');
  const inputBaseProps = buildInputBaseProps(value, label, disabled, fieldBinding);
  const modalProps = buildAiAssistantModalProps(
    { value, title: fieldName, fieldName, open: showAIAssistantModal, onClose: handleCloseAIAssistant, specifiedLanguage: detectedLanguage, modelConfig },
    { disabled, projectId, fieldBinding, fStringAutocomplete, pipelineContext },
  );

  /*
   * THE TRIGGER IS GATED HERE, not only at the call sites.
   *
   * The modal posts to `/elitea_core/predict_llm/...` in its STREAMING mode
   * (`await_task_timeout: 0`, output over an `application_predict` socket.io
   * event). The route is served now, but only its blocking mode is — the Go
   * stack has no socket task channel at all, so this trigger stays hidden.
   * `SimpleLLMInputItem` was gated on its own, but
   * this component is rendered directly by `nodes/PrinterNode.tsx`,
   * `nodes/ConditionNode.tsx` and `settings/PipelineSettings.tsx` as well.
   * Those three paths kept a button that answers 404. The gate belongs on the
   * component that owns the trigger, so a later call site cannot miss it.
   */
  const aiAssistantServed = hasBackendCapability('llmPredictStreaming');

  return (
    <Box sx={{ position: 'relative' }}>
      <InputBase
        {...inputBaseProps}
        {...(fieldName.toLowerCase() === 'code' ? { expand: { minRows: 3, maxRows: 8 } } : {})}
      />
      {aiAssistantServed && (
        <Tooltip
          title={triggerLabel}
          placement="top"
        >
          <IconButton
            onClick={handleOpenAIAssistant}
            sx={triggerButtonSx}
            aria-label={triggerLabel}
            size="small"
          >
            <SvgIcon
              component={AiAssistantIcon}
              inheritViewBox
              fontSize="small"
            />
          </IconButton>
        </Tooltip>
      )}
      {aiAssistantServed && showAIAssistantModal && <AIAssistantModal {...modalProps} />}
    </Box>
  );
});
