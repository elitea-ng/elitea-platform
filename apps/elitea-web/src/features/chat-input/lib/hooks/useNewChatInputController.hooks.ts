import type { DragEvent, Ref, RefObject } from 'react';
import { useCallback, useEffect, useImperativeHandle, useRef } from 'react';

import type { AttachmentButtonHandle, NewChatInputHandle, VoiceButtonHandle } from '../../ui/NewChatInput.types';
import type { UserInputHandle } from '../../ui/UserInput.types';

/**
 * Internal controller hooks for `NewChatInput.tsx`, split out purely to
 * keep that component's own file (and per-function cyclomatic complexity)
 * under the §3.5 budgets — same rationale as `useUserInputController
 * .hooks.ts`. None of these are components, so the §3.5 "≤12 component
 * props" row does not apply.
 */

/** Two refs `NewChatInput.tsx` shares across its controller hooks. */
export function useNewChatInputRefs(): {
  readonly userInputRef: RefObject<UserInputHandle | null>;
} {
  const userInputRef = useRef<UserInputHandle>(null);
  return { userInputRef };
}

export interface UseNewChatInputImperativeHandleParams {
  readonly ref: Ref<NewChatInputHandle>;
  readonly userInputRef: RefObject<UserInputHandle | null>;
  readonly pauseForRegeneration: () => void;
}

/**
 * Proxies every `UserInputHandle` method through to whatever `UserInput`
 * instance is currently mounted, plus the speaking-mode pause this wrapper
 * itself adds — baseline: `NewChatInput.jsx`'s own `useImperativeHandle`.
 */
export function useNewChatInputImperativeHandle(params: UseNewChatInputImperativeHandleParams): void {
  const { ref, userInputRef, pauseForRegeneration } = params;
  useImperativeHandle(
    ref,
    (): NewChatInputHandle => ({
      focus: () => userInputRef.current?.focus(),
      reset: () => userInputRef.current?.reset(),
      getInputContent: () => userInputRef.current?.getInputContent() ?? '',
      getCursorPosition: () => userInputRef.current?.getCursorPosition() ?? null,
      getLastEditPosition: () => userInputRef.current?.getLastEditPosition() ?? null,
      setValue: (...args) => userInputRef.current?.setValue(...args),
      replaceRange: (...args) => userInputRef.current?.replaceRange(...args),
      removeSymbol: (...args) => userInputRef.current?.removeSymbol(...args),
      sendQuestion: () => userInputRef.current?.sendQuestion(),
      insertTextAtCursor: (...args) => userInputRef.current?.insertTextAtCursor(...args),
      mentionUser: (...args) => userInputRef.current?.mentionUser(...args),
      pauseSpeakingMode: () => pauseForRegeneration(),
    }),
    [userInputRef, pauseForRegeneration],
  );
}

/**
 * Baseline: `NewChatInput.jsx`'s `useEffect(() => { voiceButtonRef.current
 * ?.stop(); }, [conversationId]);` — stops any active one-shot voice
 * recording whenever the active conversation changes. Ported as-is, using
 * the INJECTED `voiceButtonRef` (see `NewChatInput.types.ts`'s module doc
 * for why this is a ref, not a direct import of the real `VoiceButton`).
 */
export function useStopVoiceOnConversationChange(
  voiceButtonRef: RefObject<VoiceButtonHandle | null> | undefined,
  conversationId: string | undefined,
): void {
  useEffect(() => {
    voiceButtonRef?.current?.stop();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- baseline parity (NewChatInput.jsx): fires only on `conversationId` changing.
  }, [conversationId]);
}

export interface UseNewChatInputSendParams {
  readonly voiceButtonRef: RefObject<VoiceButtonHandle | null> | undefined;
  readonly onSend: ((question: string, inputContent: string) => void) | undefined;
}

/** Baseline `handleSend`: stop any one-shot voice recording, then forward to the real `onSend`. */
export function useNewChatInputSend(params: UseNewChatInputSendParams): (question: string, inputContent: string) => void {
  const { voiceButtonRef, onSend } = params;
  return useCallback(
    (question: string, inputContent: string) => {
      voiceButtonRef?.current?.stop();
      onSend?.(question, inputContent);
    },
    [voiceButtonRef, onSend],
  );
}

/** Baseline `handleInputChange`: forward to the caller's `onInputChange`, then notify the speaking-mode loop of the manual edit (resets its auto-send timer). */
export function useNewChatInputInputChange(
  onInputChange: ((value: string) => void) | undefined,
  notifyManualEdit: () => void,
): (value: string) => void {
  return useCallback(
    (value: string) => {
      onInputChange?.(value);
      notifyManualEdit();
    },
    [onInputChange, notifyManualEdit],
  );
}

export interface UseNewChatInputAttachmentBridgeParams {
  readonly attachmentButtonRef: RefObject<AttachmentButtonHandle | null> | undefined;
  readonly disabled: boolean;
}

/**
 * Baseline `onDrop`/`handleFilePaste`: delegate to the real attachment
 * button's own validated `onDrop` when an injected ref reaches one
 * (`refs.attachmentButtonRef` — see `NewChatInput.types.ts`'s module doc).
 * When no ref is wired (e.g. attachments hidden), baseline silently drops
 * the files — no validation-bypassing fallback path exists there, so this
 * port matches it exactly rather than forwarding raw files to
 * `onAttachFiles` unvalidated.
 *
 * **Disclosed drop**: the baseline's `disableAttachments` guard called
 * `toastError('Attachments are not allowed.')`. No toast system exists
 * anywhere in this app yet (same gap already disclosed at
 * `features/agents/ui/DeleteApplicationButton.tsx`'s own `useToast()`
 * substitution) — this silently no-ops instead.
 */
export function useNewChatInputAttachmentBridge(params: UseNewChatInputAttachmentBridgeParams): {
  readonly onDrop: (event: DragEvent<HTMLDivElement>) => void;
  readonly onFilePaste: (files: File | readonly File[]) => void;
} {
  const { attachmentButtonRef, disabled } = params;

  const onDrop = useCallback(
    (event: DragEvent<HTMLDivElement>) => {
      event.preventDefault();
      event.stopPropagation();
      if (disabled) return;
      if (attachmentButtonRef?.current) {
        const files = Array.from(event.dataTransfer.files);
        attachmentButtonRef.current.onDrop({ dataTransfer: { files }, preventDefault: () => {} });
      }
    },
    [disabled, attachmentButtonRef],
  );

  const onFilePaste = useCallback(
    (files: File | readonly File[]) => {
      if (disabled) return;
      if (attachmentButtonRef?.current) {
        const fileArray = Array.isArray(files) ? files : [files];
        attachmentButtonRef.current.onDrop({ dataTransfer: { files: fileArray }, preventDefault: () => {} });
      }
    },
    [disabled, attachmentButtonRef],
  );

  return { onDrop, onFilePaste };
}
