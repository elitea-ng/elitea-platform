import type { ReactNode, Ref } from 'react';
import { forwardRef } from 'react';

import Box from '@mui/material/Box';
import Tooltip from '@mui/material/Tooltip';

import {
  renderAttachmentList,
  resolveAttachmentsProps,
  resolveCallbacks,
  resolveField,
  resolveHighlightRanges,
  resolveMention,
  resolveSendButtonConfig,
  resolveTooltip,
  resolveVoiceProps,
} from '../lib/normalizeUserInputProps';
import {
  useUserInputChangeHandlers,
  useUserInputAutoFocus,
  useUserInputFocusHandlers,
  useUserInputInsertTextAtCursor,
  useUserInputImperativeHandle,
  useUserInputKeyHandling,
  useUserInputMentions,
  useUserInputPasteHandler,
  useUserInputRefs,
  useUserInputRowCollapse,
  useUserInputScrollSync,
  useUserInputSendQuestion,
  useUserInputTextState,
} from '../lib/hooks/useUserInputController.hooks';
import { useFileDragAndDrop } from '../lib/hooks/useFileDragAndDrop.hooks';

import type { UserInputHandle, UserInputProps } from './UserInput.types';
import { MAX_ROWS, MIN_ROWS } from './UserInput.types';
import { userInputStyles } from './UserInput.styles';
import { UserInputEditableArea } from './UserInputEditableArea';
import { UserInputFooter } from './UserInputFooter';

// Only the 4 types below are re-exported from this path (knip: `UserInput
// .test.tsx` is the one real consumer that imports via `'./UserInput'`,
// matching this component's default export site). `HighlightRange`,
// `UserInputAttachmentsProps`, `UserInputCallbacks`,
// `UserInputHighlightOverlaySlotProps`, `UserInputSendButtonConfig`,
// `UserInputSlotProps`, `UserInputSlots`, `UserInputVoiceProps` each have
// real consumers too (`../lib/normalizeUserInputProps.ts`,
// `./UserInputEditableArea.tsx`, `./UserInputFooter.tsx`, `./NewChatInput
// .types.ts`) but every one imports directly from `'./UserInput.types'`, not
// through this re-export — same rationale as `NewChatInput.tsx`'s own
// trimmed re-export block.
export type { UserInputAttachmentListSlotProps, UserInputHandle, UserInputProps, UserInputSendControlSlotProps } from './UserInput.types';

/**
 * Port of `apps/elitea-ui/src/ComponentsLib/Chat/UserInput.jsx` (unit C3,
 * "chat-input" cluster — the composition root of the whole Wave-2 chat-
 * input unit). Split across `UserInput.types.ts` (props/slots/handle
 * types), `UserInput.styles.ts` (the sx factory), `UserInputEditableArea
 * .tsx` (highlight overlay + textarea), `UserInputFooter.tsx` (send/stop
 * button area), `../lib/normalizeUserInputProps.ts` (prop-group
 * defaulting) and `../lib/hooks/useUserInputController.hooks.ts` (state/
 * handlers/imperative-handle) — purely to keep this file, and every
 * function in it, under the §3.5 budgets (≤400 lines, ≤12 cyclomatic
 * complexity, ≤12 component props, ≤3 `useEffect`s). See each split
 * file's own doc comment for why it exists.
 *
 * **Required architecture deviation** (this unit's own task brief): the
 * baseline hard-imports `SendButton` (`features/chat/ui/chat-button`, unit
 * C6) and `HighlightedText` (`features/chat/ui/highlighted-text`, unit C4)
 * — both illegal here (`no-sideways-features` is permanent, not just
 * "not yet built"). Replaced with `slots.sendControl`/`slots
 * .highlightOverlay` render-prop slots. A THIRD hard cross-unit import was
 * found while porting (not named in the task brief, discovered by reading
 * the baseline in full as instructed): `FileList`
 * (`components/Chat/FileList.jsx`) is listed under unit C4's `ownedPaths`
 * too — given the identical `no-sideways-features` constraint, it gets the
 * same treatment: `slots.attachmentList`. `slots.footer` is the baseline's
 * OWN pre-existing extension point (unrelated to the three above).
 *
 * A fourth baseline cross-file import, `./useMentionDetection` (a sibling
 * of `UserInput.jsx` in `ComponentsLib/Chat/`), is nominally listed under
 * unit C4's `ownedPaths` in `parity/wave2-partition.json` too, but is a
 * small, pure, generic text-matching hook with zero rendering — ported
 * locally instead of slotted, exactly like `useCtrlEnterKeyEventsHandler`/
 * `useFileDragAndDrop` (see `../lib/hooks/useMentionDetection.hooks.ts`'s
 * own doc comment for the full disclosure).
 */
export const UserInput = forwardRef(function UserInput(props: UserInputProps, ref: Ref<UserInputHandle>): ReactNode {
  const {
    dataTourTargetId,
    slots,
    slotProps,
    clearInputAfterSend = true,
    disabledSend = false,
    disabledInput,
    showLoading = false,
    isStreaming = false,
    isCreatingConversation = false,
  } = props;

  const { footer, sendControl, highlightOverlay, attachmentList } = slots;
  const { onSend, onStop, onNormalKeyDown, onInputChange, onFilePaste } = resolveCallbacks(props.callbacks);
  const { items: attachmentItems, onDelete: onDeleteAttachment, isUploading, uploadProgress } = resolveAttachmentsProps(
    props.attachments,
  );
  const { isSpeakingMode, isRecording, onEnterSpeakingMode, onExitSpeakingMode } = resolveVoiceProps(props.voice);
  const sp = slotProps ?? {};
  const field = resolveField(sp.field);
  const mention = resolveMention(sp.mention);
  const tooltip = resolveTooltip(sp.tooltip);
  const highlightRanges = resolveHighlightRanges(sp.highlight);
  const sendButtonConfig = resolveSendButtonConfig(sp.sendButton);

  const { inputRef, mirrorRef } = useUserInputRefs();
  const { question, setQuestion, inputContent, setInputContent, showExpandIcon, setShowExpandIcon, rows, setRows, isFocused, setIsFocused } =
    useUserInputTextState(MAX_ROWS);
  const { onFocus, onBlur } = useUserInputFocusHandlers(setIsFocused);

  // Mention-detection results are consumed entirely inside this hook's own
  // effect (bubbling `onMentionChange` + focusing the input) — nothing
  // here needs the returned array itself.
  useUserInputMentions({ inputContent, users: mention.users, onMentionChange: mention.onMentionChange, inputRef });

  const { isDragOver, handleDragOver, handleDragLeave, handleDrop } = useFileDragAndDrop(sp.container?.onDrop);

  const hasHighlights = highlightRanges.length > 0 && inputContent !== '';

  useUserInputScrollSync(inputRef, mirrorRef, hasHighlights);
  useUserInputRowCollapse(showExpandIcon, MAX_ROWS, setRows);
  useUserInputAutoFocus(inputRef, isCreatingConversation);

  const sendQuestion = useUserInputSendQuestion({
    question,
    inputContent,
    disabledSend,
    clearInputAfterSend,
    onSend,
    setQuestion,
    setInputContent,
    setShowExpandIcon,
  });
  const insertTextAtCursor = useUserInputInsertTextAtCursor({
    inputRef,
    setInputContent,
    setQuestion,
    setShowExpandIcon,
  });
  useUserInputImperativeHandle({ ref, inputRef, inputContent, setInputContent, setQuestion, setShowExpandIcon, sendQuestion, insertTextAtCursor });

  const { onInputQuestion, onClickExpander } = useUserInputChangeHandlers({
    setInputContent,
    setQuestion,
    setShowExpandIcon,
    setRows,
    onInputChange,
    maxRows: MAX_ROWS,
    minRows: MIN_ROWS,
  });
  const handlePaste = useUserInputPasteHandler(onFilePaste);
  const { onKeyDown, onKeyUp, onCompositionStart, onCompositionEnd } = useUserInputKeyHandling({
    insertTextAtCursor,
    sendQuestion,
    onNormalKeyDown,
  });

  const styles = userInputStyles(isFocused, isDragOver, isRecording);

  return (
    <Box
      sx={styles.gradientBorder}
      data-tour={dataTourTargetId}
    >
      <Tooltip
        title={tooltip.title}
        placement={tooltip.placement}
      >
        <Box
          sx={styles.container}
          onDragOver={handleDragOver}
          onDragLeave={handleDragLeave}
          onDrop={handleDrop}
        >
          {renderAttachmentList(attachmentItems, attachmentList, onDeleteAttachment, isStreaming || showLoading)}
          <UserInputEditableArea
            refs={{ inputRef, mirrorRef }}
            content={{ value: inputContent, hasHighlights, ranges: highlightRanges }}
            highlightOverlay={highlightOverlay}
            focusState={{ isFocused, onFocus, onBlur }}
            rowState={{ rows, showExpandIcon, onClickExpander }}
            handlers={{ onChange: onInputQuestion, onKeyDown, onKeyUp, onCompositionStart, onCompositionEnd, onPaste: handlePaste }}
            field={field}
            disabled={disabledInput}
            styles={styles}
          />
          <UserInputFooter
            footer={footer}
            showStop={isStreaming && !isUploading}
            sendControl={sendControl}
            sendControlProps={{
              isSpeakingMode,
              question,
              disabledSend,
              onEnterSpeakingMode,
              onExitSpeakingMode,
              onSend: sendQuestion,
              tooltipOfSendButton: sendButtonConfig.tooltipOfSendButton,
              config: sendButtonConfig,
            }}
            showLoading={showLoading}
            uploadProgress={isUploading ? uploadProgress : undefined}
            onStop={onStop}
            stopButtonConfig={sp.stopButton}
            styles={styles}
          />
        </Box>
      </Tooltip>
    </Box>
  );
});
