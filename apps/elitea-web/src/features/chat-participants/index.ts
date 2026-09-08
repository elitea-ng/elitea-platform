/**
 * Chat-participants feature — public API barrel.
 *
 * Port of `apps/elitea-ui/src/[fsd]/features/chat/participants/` (unit C5,
 * 33 files, ~4,685 LOC). Curated to ≤20 named exports per §3.3 budget.
 *
 * NOTE: This barrel is intentionally minimal. For the full API, import
 * directly from feature sub-paths (e.g. `features/chat-participants/ui/...`).
 */

// ── model (essential constants only) ──
export { ChatParticipantType } from './model/constants';

// ── lib helpers ──
export { canParticipantBeActiveInChat, isParticipantOKForChat, transformParticipant } from './lib/helpers';

// ── lib hooks ──
export { useParticipantName } from './lib/hooks/useParticipantName';
export { useParticipantEntityIcon } from './lib/hooks/useParticipantEntityIcon';
export { useActiveParticipantDetails } from './lib/hooks/useActiveParticipantDetails';
/** Wave-2 unit C6 addition: needed directly (not just through `useActiveParticipantDetails`) to fetch a NEWLY selected version's details before persisting it — see `widgets/chat-box/ui/ChatBox.tsx`'s `handleSelectVersion`. */
export { useFetchParticipantDetails } from './lib/hooks/useFetchParticipantDetails';

// ── lib context ──
export { ParticipantDetailsProvider } from './lib/context/ParticipantDetailsContext';

// ── UI (essential) ──
export { Participants } from './ui/Participants';
export { ParticipantsWrapper } from './ui/ParticipantsWrapper';
export { default as ParticipantItem } from './ui/ExpandedParticipants/ParticipantItem';
export { default as ParticipantWarning } from './ui/ExpandedParticipants/ParticipantWarning';
/** The add-participant picker. Exported for `pages/chat`, the composition root that mounts it — see gap G2 in `ParticipantsLayout.tsx`'s header comment. */
export { default as AddNewUserModal } from './ui/chat-modal/AddNewUserModal';

// ── chat hooks ──
export { default as useDeleteParticipant } from './hooks/chat/useDeleteParticipant';
export { default as useLocalActiveParticipant } from './hooks/chat/useLocalActiveParticipant';
export { default as useRemoteParticipantUpdate } from './hooks/chat/useRemoteParticipantUpdate';
