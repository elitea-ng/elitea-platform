import type { RefObject } from 'react';
import { forwardRef, memo, useCallback, useEffect, useImperativeHandle, useRef } from 'react';

import MicIcon from '@mui/icons-material/Mic';
import StopIcon from '@mui/icons-material/Stop';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';

import { voiceHooks } from '@/features/chat-input';
import { useVoiceFeatureFlags } from '@/shared/lib/hooks/useVoiceFeatureFlags';
import { t } from '@/shared/i18n';

/**
 * Chat button primitive: VoiceButton
 *
 * Real dictation: starts/stops speech recognition and writes the recognized
 * text into the chat input at the cursor position the user had when they
 * clicked the mic (`inputRef`), exactly like baseline `VoiceButton.jsx`.
 * Prefers a project-scoped backend ASR model
 * (`voiceHooks.useStreamingSpeechRecognition`, selected via `projectId`) and
 * falls back to the browser's own Web Speech API
 * (`voiceHooks.useSpeechRecognition`) when no ASR model is configured or
 * `projectId` isn't supplied yet — same `serverHook.isSupported ? serverHook
 * : clientHook` priority baseline uses. Renders nothing when neither is
 * available (matches baseline's `if (!isSupported) return null`).
 *
 * `inputRef` is a ref-shaped handle (not a plain string callback) —
 * deliberately matching this codebase's established convention for
 * chat-textarea interop (`ChatInputHandle`/`SpeakingModeInputHandle` in
 * `features/chat-input`): structurally compatible with both, so any real
 * textarea ref built against either shape satisfies this prop with no
 * adapter.
 *
 * `onError` replaces baseline's direct `useToast().toastError(...)` call —
 * no toast/snackbar primitive exists yet in `shared/ui` (established gap,
 * see `features/mcps/model/useMcpAuthModal.ts`'s identical disclosure); the
 * caller decides how to surface the message until one lands.
 *
 * The two voice flags are READ FROM THE PLATFORM (`useVoiceFeatureFlags`,
 * A14/issue 200), not hardcoded. They used to be `VOICE_FEATURES_ENABLED =
 * true` / `VOICE_FEATURES_TEMPORARILY_DISABLED = false` module constants,
 * disclosed as a `shared/config` gap — but the gap was not in `shared/config`:
 * the admin Features page has always offered switches named after both, and
 * they reached nothing. `GET /elitea_core/platform_settings/…` now marshals
 * them from `centry.platform_config`.
 *
 * Prop contract (injected by the composition root through `slots.voiceButton`):
 *   - `disabled`          — disables the button entirely
 *   - `onRecordingChange` — callback fired every time recording state flips
 *   - `onError`           — fired with a human-readable message on mic/ASR errors
 *   - `inputRef`          — ref into the chat textarea for cursor-aware transcript insertion
 *   - `projectId`         — enables backend streaming ASR when a project ASR model exists
 *
 * Imperative handle (`VoiceButtonHandle`):
 *   - `stop()` — halts the in-progress recording
 */
export interface VoiceButtonHandle {
  stop(): void;
}

export interface VoiceButtonInputHandle {
  getInputContent(): string;
  getCursorPosition(): number | null;
  /**
   * The caret position as of the last real EDIT (issue #933, ELITEA-1317).
   * Optional: a host whose composer does not track it falls back to
   * `getCursorPosition()`, which is what this component used to read
   * unconditionally.
   */
  getLastEditPosition?(): number | null;
  setValue(value: string, cursorPosition: number): void;
  focus?(): void;
}

/**
 * Where a dictation session should splice its text into `content` (#933).
 *
 * The LAST EDITED position wins over the live caret: a person who types a
 * sentence, clicks somewhere in the middle of it to re-read a word, and then
 * reaches for the mic means the dictation to continue where they were
 * WRITING, not where the pointer happened to land. Clamped, because the
 * recorded position can outlive an edit that shortened the text.
 */
export function resolveDictationInsertPoint(handle: VoiceButtonInputHandle | null | undefined, content: string): number {
  const lastEdit = handle?.getLastEditPosition?.() ?? null;
  const cursor = lastEdit ?? handle?.getCursorPosition() ?? content.length;
  return Math.min(Math.max(cursor, 0), content.length);
}

/**
 * The text before the insertion point, with a single separating space added
 * when the transcript would otherwise be welded onto the end of a word
 * (#933). Dictation is speech: "Hello world" + "beautiful" is three words,
 * not "worldbeautiful".
 */
export function withDictationSeparator(preCursor: string): string {
  if (preCursor === '' || /\s$/.test(preCursor)) return preCursor;
  return `${preCursor} `;
}

export interface VoiceButtonProps {
  disabled?: boolean;
  onRecordingChange?: (isRecording: boolean) => void;
  onError?: (message: string) => void;
  inputRef?: RefObject<VoiceButtonInputHandle | null>;
  projectId?: string;
}


/** Maps a speech-recognition error code to human-readable text — split out of `handleVoiceError` purely so each message can carry its own `t()` call. Unmapped codes (e.g. 'no-speech'/'aborted') resolve to `undefined`, same as the plain-object lookup this replaces. */
function voiceErrorMessage(error: string): string | undefined {
  switch (error) {
    case 'not-allowed':
      return t(
        'widgets.chat.voiceButton.errorNotAllowed',
        'Microphone access denied. Please allow microphone access in your browser settings.',
      );
    case 'audio-capture':
      return t(
        'widgets.chat.voiceButton.errorAudioCapture',
        'No microphone found. Please connect a microphone and try again.',
      );
    case 'network':
      return t(
        'widgets.chat.voiceButton.errorNetwork',
        'Voice input requires an internet connection. Please check your connection and try again.',
      );
    default:
      return undefined;
  }
}

interface VoiceCursorRefs {
  readonly preCursor: RefObject<string>;
  readonly postCursor: RefObject<string>;
  readonly voiceFinal: RefObject<string>;
  readonly lastSetValue: RefObject<string | null>;
}

/**
 * Re-syncs the cursor refs if the user manually edited the input between
 * transcript events (e.g. deleted the previous transcript while still
 * recording) — same re-sync `useSpeakingModeLoop.ts`'s
 * `resyncCursorsOnManualEdit` does for its own cursor refs. Split out of
 * `handleTranscript` purely to keep that callback under the §3.5
 * cyclomatic-complexity-12 budget — same logic, no behavior change.
 */
function resyncCursorsOnManualEdit(handle: VoiceButtonInputHandle | null | undefined, refs: VoiceCursorRefs): void {
  if (refs.lastSetValue.current === null) return;
  const currentContent = handle?.getInputContent() ?? '';
  if (currentContent === refs.lastSetValue.current) return;
  const cursor = handle?.getCursorPosition() ?? currentContent.length;
  refs.preCursor.current = currentContent.slice(0, cursor);
  refs.postCursor.current = currentContent.slice(cursor);
  refs.voiceFinal.current = '';
}

/** Writes `value` into the input and records it so the next transcript event can detect a manual edit — shared by the interim/final branches of `handleTranscript`. */
function writeTranscript(
  handle: VoiceButtonInputHandle | null | undefined,
  refs: VoiceCursorRefs,
  value: string,
  cursorPosition: number,
): void {
  refs.lastSetValue.current = value;
  handle?.setValue(value, cursorPosition);
}

interface VoiceButtonUiState {
  readonly micDisabled: boolean;
  readonly tooltipTitle: string;
  readonly iconColor: 'primary' | 'secondary';
  readonly ariaLabel: string;
  readonly wrapperGap: number;
  readonly wrapperBorder: string;
  readonly wrapperBorderRadius: string | number;
}

/** All of the render's derived (disabled/color/label/wrapper-shape) state in one place — split out purely to keep the component itself under the §3.5 cyclomatic-complexity-12 budget. */
function deriveVoiceButtonUiState(disabled: boolean, isRecording: boolean, isAdminDisabled: boolean): VoiceButtonUiState {
  const tooltipTitle = isAdminDisabled
    ? t('widgets.chat.voiceButton.tooltipAdminDisabled', 'Temporarily disabled by admin')
    : isRecording
      ? t('widgets.chat.voiceButton.tooltipRecording', 'Voice input active')
      : t('widgets.chat.voiceButton.tooltipStart', 'Start voice input');
  return {
    micDisabled: disabled || isRecording || isAdminDisabled,
    tooltipTitle,
    iconColor: isRecording ? 'primary' : 'secondary',
    ariaLabel: isRecording
      ? t('widgets.chat.voiceButton.ariaLabelRecording', 'voice input active')
      : t('widgets.chat.voiceButton.ariaLabelStart', 'start voice input'),
    wrapperGap: isRecording ? 0.5 : 0,
    wrapperBorder: isRecording ? '0.0625rem solid' : 'none',
    wrapperBorderRadius: isRecording ? '1.75rem' : 0,
  };
}

export const VoiceButton = memo(
  forwardRef<VoiceButtonHandle, VoiceButtonProps>(
    ({ disabled = false, onRecordingChange, onError, inputRef, projectId }, ref) => {
      const voiceFlags = useVoiceFeatureFlags();
      // Text before/after the cursor at the moment recording started, plus the
      // finalized voice text accumulated so far this session — same 3-ref
      // cursor-aware-insertion scheme as baseline `VoiceButton.jsx`.
      const preCursorContentRef = useRef('');
      const postCursorContentRef = useRef('');
      const voiceFinalAccumulatedRef = useRef('');
      // Last value we programmatically wrote — detects manual edits made
      // between transcript events so we can re-sync instead of clobbering them.
      const lastSetValueRef = useRef<string | null>(null);
      const cursorRefs: VoiceCursorRefs = {
        preCursor: preCursorContentRef,
        postCursor: postCursorContentRef,
        voiceFinal: voiceFinalAccumulatedRef,
        lastSetValue: lastSetValueRef,
      };

      const handleTranscript = useCallback(
        ({ final, interim }: { final: string; interim: string }) => {
          const handle = inputRef?.current;
          resyncCursorsOnManualEdit(handle, cursorRefs);

          const voiceBase = preCursorContentRef.current + voiceFinalAccumulatedRef.current;
          if (interim) {
            const newValue = voiceBase + interim + postCursorContentRef.current;
            writeTranscript(handle, cursorRefs, newValue, voiceBase.length + interim.length);
          }
          if (final) {
            voiceFinalAccumulatedRef.current += (voiceFinalAccumulatedRef.current ? ' ' : '') + final;
            const newValue =
              preCursorContentRef.current + voiceFinalAccumulatedRef.current + postCursorContentRef.current;
            const cursorPos = preCursorContentRef.current.length + voiceFinalAccumulatedRef.current.length;
            writeTranscript(handle, cursorRefs, newValue, cursorPos);
          }
        },
        // oxlint-disable-next-line react/exhaustive-deps -- `cursorRefs` wraps stable `useRef` containers into a fresh plain object every render (ref identity itself never changes); only `inputRef` (a caller-supplied prop) is a real dependency, same reasoning `useSpeakingModeLoop.ts`'s own ref-bundling callbacks apply throughout that file.
        [inputRef],
      );

      const handleVoiceError = useCallback(
        (error: string) => {
          const message = voiceErrorMessage(error);
          // 'no-speech'/'aborted' are silently ignored — not user-facing errors (baseline parity).
          if (message) onError?.(message);
        },
        [onError],
      );

      const { data: asrModelsData } = voiceHooks.useModelsList(
        { projectId, section: 'asr', includeShared: true },
        { enabled: projectId !== undefined },
      );
      const asrModel = voiceHooks.selectAsrModel(asrModelsData?.items ?? []);

      const serverHook = voiceHooks.useStreamingSpeechRecognition({
        onTranscript: handleTranscript,
        onError: handleVoiceError,
        projectId,
        asrModel,
      });

      const clientHook = voiceHooks.useSpeechRecognition({
        onTranscript: handleTranscript,
        onError: handleVoiceError,
      });

      // Priority: backend streaming ASR model, then the browser Speech API.
      const { isRecording, isSupported, startRecording, stopRecording } = serverHook.isSupported
        ? serverHook
        : clientHook;

      useEffect(() => {
        onRecordingChange?.(isRecording);
      }, [isRecording, onRecordingChange]);

      const handleStartRecording = useCallback(() => {
        const handle = inputRef?.current;
        const content = handle?.getInputContent() ?? '';
        const cursor = resolveDictationInsertPoint(handle, content);
        preCursorContentRef.current = withDictationSeparator(content.slice(0, cursor));
        postCursorContentRef.current = content.slice(cursor);
        voiceFinalAccumulatedRef.current = '';
        lastSetValueRef.current = content;
        // `startRecording` is `(() => Promise<void>) | (() => void)` — the
        // union of both ASR hooks' signatures, same as `useSpeakingModeLoop
        // .ts`'s `beginRecording` (no floating-promise `void` needed; it
        // resolves to plain `void` either way).
        startRecording();
      }, [inputRef, startRecording]);

      // The focus hand-back belongs to a recording that was actually running:
      // it returns the caret to the composer after the transcript lands. But
      // `stop()` is ALSO called with nothing recording — `NewChatInput`'s
      // `useStopVoiceOnConversationChange` fires it on mount and on every
      // conversation change ("stops any ACTIVE one-shot recording"). Ungated,
      // that steals focus into the composer on cold load and on every
      // conversation switch, which on `/chat` blanks the placeholder
      // (`UserInputEditableArea` renders none while focused) and on mobile pops
      // the keyboard. It went unnoticed only because `ChatBox` was clobbering
      // this ref, so `focus?.()` was a silent no-op (PR #597).
      const handleStopRecording = useCallback(() => {
        stopRecording();
        if (isRecording) inputRef?.current?.focus?.();
      }, [inputRef, stopRecording, isRecording]);

      useImperativeHandle(ref, () => ({ stop: handleStopRecording }), [handleStopRecording]);

      if (!isSupported) return null;
      // The platform switch, read rather than hardcoded (A14, issue 200). These
      // were `const VOICE_FEATURES_ENABLED = true` / `= false` module
      // constants, so the admin Features page's Voice Features section named
      // this control and could not change it. This component is the MOUNTED
      // voice control — `/chat` → `ChatBox` → `buildChatBoxInputSlots()` — which
      // is what makes that section live rather than withheld.
      if (!voiceFlags.enabled) return null;

      const ui = deriveVoiceButtonUiState(disabled, isRecording, voiceFlags.temporarilyDisabled);

      return (
        <Box
          component="span"
          sx={{
            display: 'flex',
            alignItems: 'center',
            gap: ui.wrapperGap,
            border: ui.wrapperBorder,
            borderColor: 'border.chatContinue',
            borderRadius: ui.wrapperBorderRadius,
            boxSizing: 'border-box',
          }}
        >
          <Tooltip title={ui.tooltipTitle} placement="top">
            <Box component="span">
              <IconButton
                color={ui.iconColor}
                aria-label={ui.ariaLabel}
                aria-pressed={isRecording}
                disabled={ui.micDisabled}
                onClick={ui.micDisabled ? undefined : handleStartRecording}
                sx={{ marginLeft: 0 }}
              >
                <MicIcon fontSize="small" />
              </IconButton>
            </Box>
          </Tooltip>

          {isRecording && (
            <Tooltip title={t('widgets.chat.voiceButton.stopDictationTitle', 'Stop dictation')} placement="top">
              <Box component="span">
                <IconButton
                  color="secondary"
                  aria-label={t('widgets.chat.voiceButton.stopVoiceInputAriaLabel', 'stop voice input')}
                  onClick={handleStopRecording}
                  disabled={disabled}
                  sx={{ marginLeft: 0 }}
                >
                  <StopIcon fontSize="small" />
                </IconButton>
              </Box>
            </Tooltip>
          )}
        </Box>
      );
    },
  ),
);

VoiceButton.displayName = 'VoiceButton';
