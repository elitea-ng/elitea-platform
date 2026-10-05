/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20
 * named exports, TYPE specifiers count too — `scripts/lib/budgets-core.mjs`'s
 * `countExports` sums every specifier in every `export {...}`/`export type
 * {...}` statement). This slice (`features/chat-input`) was built as 6
 * parallel Wave-2 C3 sub-clusters (core-input, voice-asr, voice-tts-config,
 * slash-mention, attachments-ui, conversation-starters) plus 2 prerequisite
 * phases — this file is the FINAL integration pass across all of them
 * (built last, with full visibility other clusters didn't have), not an
 * additive per-cluster stack anymore. Rewritten from the ground up against
 * every sub-cluster's actual landed code (verified via `find src/features/
 * chat-input -name '*.ts*'`), not the plan.
 *
 * **Cross-cluster fix landed alongside this barrel** (not just a barrel
 * edit): `ui/VoiceControlButton.tsx` originally invented its own
 * `renderVoiceConfigDialog` injected slot because `ui/VoiceConfigDialog.tsx`
 * (built by the sibling "voice-tts-config" cluster) hadn't landed yet as of
 * that file's own build. `VoiceConfigDialog.tsx`'s own module doc states the
 * opposite contract ("`VoiceControlButton.jsx` renders this dialog... this
 * export name and prop shape are the coordination contract"), and
 * `useReadAloud.hooks.ts`'s own `VoicePlayerProps` — documented as "the
 * props a caller spreads onto its voice mini-player / control button" — has
 * no `renderVoiceConfigDialog` field at all, so the two sub-clusters'
 * contracts could never have been spread onto each other. Fixed by making
 * `VoiceControlButton` import and render `VoiceConfigDialog` directly (see
 * that file's own module doc for the full account) and aligning
 * `VoiceControlButtonProps` to be type-identical to `VoicePlayerProps`.
 * `VoiceControlButton.test.tsx`/`VoiceMiniPlayer.test.tsx` were updated to
 * match (drive the real dialog, not a fake render-prop stub).
 *
 * The OTHER flagged cross-cluster risk — a duplicate models-list-by-section
 * fetcher between voice-asr/voice-tts-config — is confirmed NOT a real
 * duplicate: only one `api/models.ts` exists in this slice (ASR built it
 * first, parameterized by `section`), and `lib/hooks/useTextToSpeech.types
 * .ts`'s `TtsModel` is a plain type alias of its `ModelListItem` (`export
 * type TtsModel = ModelListItem`), not a re-derived shape. No fix needed.
 * `NewChatInput.tsx`'s declared `slots.{sendControl,highlightOverlay,
 * attachmentButton,internalToolsConfig,voiceButton,modelSelector}` were
 * cross-checked against `NewChatInputFooterContent.tsx`'s actual
 * `slots.*` reads and `NewChatInput.types.ts`'s `NewChatInputSlots` — all
 * six names match exactly; no mismatch found there either.
 *
 * **Budget strategy**: every component below also gets its own top-level
 * export slot (JSX component identity can't be bundled into a shared
 * object the way plain functions/hooks can — no precedent for that
 * anywhere in this codebase, and inventing one here isn't worth the
 * novelty). Every HOOK group, by contrast, is bundled into ONE
 * object-literal export per theme — bundling costs exactly 1 slot no
 * matter how many members it carries (`entities/conversation`'s
 * `conversationApi`/`chatHelpers`/etc. are the established precedent for
 * this at a much larger scale: "6 of the 13 are curated OBJECT BUNDLES...
 * rather than ~30 individual named exports"). A bundled hook is still a
 * plain function reachable by member access and still callable as a hook
 * from component top-level (`conversationApi.useEdit()` is a real,
 * lint-clean call site already in this codebase, at `processes/chat/model/
 * useInternalToolsConfig.ts`) — bundling changes nothing about how a
 * caller uses each hook, only how it's imported.
 *
 * Every param/result TYPE for a bundled hook is deliberately NOT
 * re-exported here unless it is a genuine cross-slice CONTRACT a caller
 * must independently type something against before calling the hook
 * (an injected ref handle, a query-function shape, a participant "gate"
 * shape) — plain param/result bags are reachable via `Parameters<typeof
 * theBundle.theHook>[0]`/`ReturnType<typeof theBundle.theHook>` derivation,
 * the same "entities/application-form-precedented" convention this slice's
 * slash-mention section already established, or via `ComponentProps<typeof
 * X>`/`ComponentRef<typeof X>` for an exported component's own props/
 * imperative-handle shape (`features/toolkits/index.ts`'s own doc comment
 * sets this exact precedent: "derives the one shared shape it DOES need to
 * name... via `ComponentProps<typeof ToolkitForm>` instead of a fifth type
 * export"). `UserInput` itself stays intra-slice-only — `NewChatInput` (its
 * only real wrapper) is the sole composition-root entry point; nothing
 * outside this slice plausibly renders the bare textarea directly.
 * `EllipsisTextWithTooltip` (a small presentational atom, used today only
 * by `ChatConversationStarters`) and `VoiceConfigDialog`/
 * `VoicePersonalizationSection` (the voice-tts-config settings-page pieces,
 * now wired directly by `VoiceControlButton` — see above) are likewise kept
 * intra-slice-only for now: real budget pressure from the required
 * voice/attachments additions below left no room, and none has a
 * disclosed external caller today. Promote any of them the moment a real
 * cross-slice consumer needs one directly.
 */
import { deriveConversationStarters, useConversationStarters } from './lib/useConversationStarters';
import { useConversationStartersSync } from './lib/useConversationStartersSync';
import { useChatSkillMention } from './lib/hooks/useChatSkillMention';
import { useSlashMention } from './lib/hooks/useSlashMention';
import { selectAsrModel, useSpeakingModeLoop } from './lib/hooks/useSpeakingModeLoop';
import { useSpeechRecognition } from './lib/hooks/useSpeechRecognition';
import { useStreamingSpeechRecognition } from './lib/hooks/useStreamingSpeechRecognition';
import { useReadAloud } from './lib/hooks/useReadAloud.hooks';
import { voiceErrorMessage } from '@/shared/lib/voiceProblems';
import { useModelsList } from './api/models';
import { useChatAttachments } from './model/useChatAttachments';
import { useNewConversationAttachments } from './model/useNewConversationAttachments';

// ---------------------------------------------------------------------------
// conversation-starters sub-cluster: the empty-chat "click a starter to
// send" display. Ported from `apps/elitea-ui/src/pages/NewChat/
// ChatConversationStarters.jsx` + `.../[fsd]/features/chat/lib/hooks/
// {useConversationStarters,useConversationStartersSync}.hooks.js`.
// ---------------------------------------------------------------------------
export { ChatConversationStarters } from './ui/ChatConversationStarters';
export type { ChatConversationStartersProps } from './ui/ChatConversationStarters';
export type { ConversationStartersParticipantSnapshot } from './lib/useConversationStarters';

/**
 * `useConversationStartersSync` is the real implementation of the DI slot
 * `features/agents/ui/AgentEditor.tsx` declares as `AgentEditorDeps
 * .useConversationStartersSync` (defaulted there to a no-op) — member
 * access (`conversationStarterHooks.useConversationStartersSync`) is still
 * a plain function reference, so it wires into that slot unchanged.
 * `UseConversationStartersParams`/`Result` stay unexported (derivable via
 * `Parameters<typeof conversationStarterHooks.useConversationStarters>[0]`/
 * `ReturnType<...>`) — `ConversationStartersParticipantSnapshot` above is
 * kept individually since a caller needs to name it to write its own
 * `Participant`/`ApplicationDetail` -> snapshot mapping function.
 */
export const conversationStarterHooks = {
  useConversationStarters,
  useConversationStartersSync,
  deriveConversationStarters,
};

// ---------------------------------------------------------------------------
// slash-mention sub-cluster: "/" toolkit-slash, "~" skill-mention, "@"
// user-mention suggestion systems.
// ---------------------------------------------------------------------------

/**
 * `useSlashCommandHandler`/`useSlashHighlights` (internal building blocks of
 * `useSlashMention`) and `ToolkitValidator`/`ToolkitMentionList` (internal
 * building blocks of `SlashSuggestionList`) stay intra-slice — confirmed
 * against the baseline that none of the four has a caller outside the
 * component/hook that already lives in this slice. Every supporting
 * param/result type (`UseSlashMentionParams`/`Result`,
 * `UseChatSkillMentionParams`/`Result`, `SlashParticipantToolkit`,
 * `SlashPhase`, `CommittedToolkitMention`, `SkillMentionItem`,
 * `CommittedSkillMention`, `MentionRange`, …) is likewise intra-slice-only:
 * reachable via `Parameters<typeof mentionHooks.useSlashMention>[0]`/
 * `ReturnType<...>` derivation when a real cross-slice caller needs to name
 * one directly.
 */
export const mentionHooks = {
  useSlashMention,
  useChatSkillMention,
};

export { SlashSuggestionList } from './ui/SlashSuggestionList';
export type { UseToolkitDetailsQuery } from './ui/SlashSuggestionList';
export { UserMentionList } from './ui/UserMentionList';
export type { ChatInputHandle } from './lib/chatInputHandle';

// ---------------------------------------------------------------------------
// core-input sub-cluster: `UserInput`/`NewChatInput` composition root.
// `NewChatInputHandle`/`NewChatInputProps` stay unexported — derivable as
// `ComponentRef<typeof NewChatInput>`/`ComponentProps<typeof NewChatInput>`
// (React 19; `features/toolkits/index.ts`'s own doc comment sets this exact
// precedent for a forwardRef component's handle/props shape).
// ---------------------------------------------------------------------------
export { NewChatInput } from './ui/NewChatInput';
export { chatInputCompositionHooks } from './lib/hooks/chatInputCompositionHooks';

// ---------------------------------------------------------------------------
// voice-asr / voice-tts-config sub-clusters: the hands-free speaking-mode
// loop, read-aloud/TTS glue, and the play/stop + settings control shared by
// every surface that renders a voice mini-player.
// ---------------------------------------------------------------------------
export type { SpeakingModeInputHandle } from './lib/hooks/useSpeakingModeLoop';

/**
 * `VoicePlayerProps`/`UseReadAloudParams`/`Result` (`useReadAloud.hooks.ts`)
 * stay unexported: a caller destructures `useReadAloud(...)`'s result and
 * spreads its `voicePlayerProps` field straight onto `VoiceControlButton`/
 * `VoiceMiniPlayer` below with no explicit type needed (TS checks
 * structurally) — see `VoiceControlButton.tsx`'s own module doc for why
 * `VoiceControlButtonProps` is now type-identical to `VoicePlayerProps`.
 * `SpeakingModeInputHandle` (above) is the one real contract a caller must
 * independently type its own textarea ref against before calling
 * `voiceHooks.useSpeakingModeLoop`.
 *
 * `useSpeechRecognition`/`useStreamingSpeechRecognition`/`useModelsList`/
 * `selectAsrModel` are bundled in here too (widget-C6 `VoiceButton`'s
 * consumers) — they were previously intra-slice-only building blocks of
 * `useSpeakingModeLoop` itself; `VoiceButton` needs the same client/server
 * ASR priority `useSpeakingModeLoop` already implements, but for one-shot
 * cursor-aware dictation rather than the hands-free auto-send loop, so it
 * calls the two ASR hooks directly instead of going through
 * `useSpeakingModeLoop`. Adding these as new MEMBERS of the existing
 * `voiceHooks` object costs zero additional slots against the §3.5
 * `slice-public-api` budget (`scripts/lib/budgets-core.mjs`'s `countExports`
 * counts the one `export const voiceHooks` declaration, not its members).
 */
export const voiceHooks = {
  useSpeakingModeLoop,
  useReadAloud,
  useSpeechRecognition,
  useStreamingSpeechRecognition,
  useModelsList,
  selectAsrModel,
  /** The readable message for a server voice failure code (`shared/lib/voiceProblems.ts`); `undefined` for a browser error name. */
  voiceErrorMessage,
};

export { VoiceControlButton } from './ui/VoiceControlButton';
export type { VoiceControlButtonProps } from './ui/VoiceControlButton';
export { VoiceMiniPlayer } from './ui/VoiceMiniPlayer';

// ---------------------------------------------------------------------------
// attachments-ui sub-cluster: the chat-attachment thumbnail/viewer, and the
// two attachment-composing hooks a future send-box composition root needs
// (existing-conversation vs. not-yet-created-conversation variants).
// ---------------------------------------------------------------------------
export { ImageAttachment } from './ui/ImageAttachment';
export type { ChatAttachmentsParticipantGate, ChatAttachmentsParticipantDetailsGate } from './model/chatAttachments.types';

/**
 * `ImageAttachmentProps` stays unexported — derivable as `ComponentProps
 * <typeof ImageAttachment>` (same `features/toolkits/index.ts`-precedented
 * convention as `NewChatInput` above). `UseChatAttachmentsParams`/`Result`
 * and `UseNewConversationAttachmentsParams`/`Result`/
 * `NewConversationSelectedParticipant` stay unexported (`Parameters<typeof
 * chatAttachmentHooks.useChatAttachments>[0]`/`ReturnType<...>`
 * derivation) — the two `ChatAttachmentsParticipant*Gate` shapes above are
 * kept individually since BOTH hooks' params need them named to build the
 * `activeParticipant`/`activeParticipantDetails`/`selectedParticipant`
 * mapping a real caller writes.
 */
export const chatAttachmentHooks = {
  useChatAttachments,
  useNewConversationAttachments,
};
