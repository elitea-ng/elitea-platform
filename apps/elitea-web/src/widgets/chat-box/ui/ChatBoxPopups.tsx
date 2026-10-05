/**
 * Split out of `ChatBox.tsx` to stay under the file-length/component-props/
 * complexity budgets (§3.5) — the conditionally-rendered popups above
 * `NewChatInput`: the participants recommendation list, "@" user-mention
 * list, "/" toolkit slash-suggestion list, and "~" skill-mention list.
 * Props are grouped into option objects (one per popup) to stay under the
 * component-props budget, matching the pattern established across this Wave.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import { SlashSuggestionList, UserMentionList } from '@/features/chat-input';
import { RecommendationList } from '@/features/chat-recommendations';
import { MentionToolList } from '@/shared/ui/MentionToolList';

import { optField } from './ChatBox.helpers';
import { HashParticipantPopup } from './HashParticipantPopup';
import type { HashParticipantSelection } from './HashParticipantPopup';
import { useNoopValidateToolkitQuery, useSlashToolkitDetailsQuery } from './hooks/useChatBoxSlashQueries';
import type { useChatBoxState } from './hooks/useChatBoxState';

type ChatBoxState = ReturnType<typeof useChatBoxState>;

interface ChatBoxPopupsRecommendations {
  readonly show: boolean;
  readonly onSelectParticipant: (participant: unknown) => void;
  readonly onClose: () => void;
  readonly existingParticipants: readonly unknown[];
  readonly projectId: string | undefined;
}

interface ChatBoxPopupsUserMentions {
  readonly isProcessingAtSymbol: boolean;
  readonly hasOtherUsers: boolean;
  readonly users: ChatBoxState['users'];
  readonly atQuery: string;
  readonly onSelectUser: (user: unknown) => void;
  readonly onClose: () => void;
}

interface ChatBoxPopupsSkill {
  readonly isActive: boolean;
  readonly filteredItems: ChatBoxState['skill']['filteredItems'];
  readonly highlightedIndex: ChatBoxState['skill']['highlightedIndex'];
  readonly onSelectTool: (toolName: string | null) => void;
}

interface ChatBoxPopupsHashPicker {
  readonly isOpen: boolean;
  readonly query: string;
  readonly projectId: string | undefined;
  readonly onSelect: (selection: HashParticipantSelection) => void;
  readonly onClose: () => void;
}

export interface ChatBoxPopupsProps {
  readonly recommendations: ChatBoxPopupsRecommendations;
  readonly hashPicker?: ChatBoxPopupsHashPicker | undefined;
  readonly userMentions: ChatBoxPopupsUserMentions;
  readonly slash: ChatBoxState['slash'];
  readonly skill: ChatBoxPopupsSkill;
}

/**
 * Builds this component's four prop bundles from the one `ChatBoxState` they
 * all read, plus the three callbacks that are not part of it.
 *
 * Lives here rather than inline at the call site because `ChatBox.tsx` is at
 * its §3.5 `max-lines` ceiling and this literal was ~25 of them — and because
 * every field below is a mechanical projection of `state`, which is exactly
 * the kind of thing that belongs next to the props it feeds.
 */
export function buildChatBoxPopupsProps(input: {
  readonly state: ChatBoxState;
  readonly onChangeParticipant: ((participant: unknown) => void) | undefined;
  readonly existingParticipants: readonly unknown[];
  readonly projectId: string | undefined;
  readonly onSelectUser: ChatBoxPopupsUserMentions['onSelectUser'];
  readonly onSelectTool: ChatBoxPopupsSkill['onSelectTool'];
  /** Attaches a "#"-picked agent or pipeline (the "+" menu's `onSelectParticipant`). */
  readonly onAddParticipant?: ((selection: unknown) => void) | undefined;
}): ChatBoxPopupsProps {
  const { state, onChangeParticipant, existingParticipants, projectId, onSelectUser, onSelectTool, onAddParticipant } = input;
  return {
    hashPicker: {
      isOpen: state.keyDown.isProcessingSymbols && onAddParticipant !== undefined,
      query: state.keyDown.query,
      projectId,
      onSelect: (selection) => { state.clearHashQuery(); onAddParticipant?.(selection); },
      onClose: state.keyDown.stopProcessingSymbols,
    },
    recommendations: {
      show: state.showRecommendationList,
      onSelectParticipant: (p) => { onChangeParticipant?.(p); state.setShowRecommendationList(false); },
      onClose: () => state.setShowRecommendationList(false),
      existingParticipants,
      projectId,
    },
    userMentions: {
      isProcessingAtSymbol: state.keyDown.isProcessingAtSymbol,
      hasOtherUsers: state.hasOtherUsers,
      users: state.users,
      atQuery: state.keyDown.atQuery,
      onSelectUser,
      onClose: state.keyDown.stopProcessingAtSymbol,
    },
    slash: state.slash,
    skill: {
      isActive: state.isSkillPhaseActive,
      filteredItems: state.skill.filteredItems,
      highlightedIndex: state.skill.highlightedIndex,
      onSelectTool,
    },
  };
}

export function ChatBoxPopups({ recommendations, hashPicker, userMentions, slash, skill }: ChatBoxPopupsProps): ReactNode {
  return (
    <>
      {hashPicker?.isOpen === true && (
        <HashParticipantPopup
          query={hashPicker.query}
          projectId={hashPicker.projectId}
          onSelect={hashPicker.onSelect}
          onClose={hashPicker.onClose}
        />
      )}
      {recommendations.show && (
        <Box sx={{ mb: 1 }}>
          <RecommendationList
            onSelectParticipant={recommendations.onSelectParticipant}
            existingParticipants={[...recommendations.existingParticipants]}
            onClose={recommendations.onClose}
            projectId={recommendations.projectId}
          />
        </Box>
      )}
      {userMentions.isProcessingAtSymbol && userMentions.hasOtherUsers && (
        <Box sx={{ mb: 1 }}>
          <UserMentionList
            users={userMentions.users}
            query={userMentions.atQuery}
            onSelectUser={userMentions.onSelectUser}
            onClose={userMentions.onClose}
          />
        </Box>
      )}
      {slash.phase !== 'idle' && (
        <Box sx={{ mb: 1 }}>
          <SlashSuggestionList
            phase={slash.phase}
            toolkitQuery={slash.toolkitQuery}
            toolQuery={slash.toolQuery}
            selectedToolkit={slash.selectedToolkit}
            isQueryFinal={slash.isQueryFinal}
            onSelectToolkit={slash.onSelectToolkit}
            onSelectTool={slash.onCommitMention}
            onClose={slash.resetSlash}
            participantToolkits={slash.participantToolkits}
            isMcpVisible={slash.isMcpVisible}
            activeIndex={slash.activeIndex}
            setActiveIndex={slash.setActiveIndex}
            itemCountRef={slash.itemCountRef}
            onConfirmActiveRef={slash.onConfirmActiveRef}
            useValidateToolkitQuery={useNoopValidateToolkitQuery}
            useToolkitDetailsQuery={useSlashToolkitDetailsQuery}
          />
        </Box>
      )}
      {skill.isActive && (
        <Box sx={{ mb: 1 }}>
          <MentionToolList
            tools={skill.filteredItems.map((item) => ({ name: item.name, ...optField('description', item.description) }))}
            toolkitName="Skill"
            onSelectTool={skill.onSelectTool}
            highlightedIndex={skill.highlightedIndex}
          />
        </Box>
      )}
    </>
  );
}
