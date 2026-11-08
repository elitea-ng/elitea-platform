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
import { t } from '@/shared/i18n';
import { MentionToolList } from '@/shared/ui/MentionToolList';
import type { MentionTool } from '@/shared/ui/MentionToolList';

import type { AddressableAgent } from '@elitea/client-core/conversation';

import { optField } from './ChatBox.helpers';
import { agentCandidates, agentOfCandidate } from './hooks/useChatBoxAgentMentions';
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
  readonly users: readonly { readonly id: string; readonly name: string }[];
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

interface ChatBoxPopupsAgents {
  /** The conversation's addressable agents (two or more), or none. */
  readonly agents: readonly AddressableAgent[];
  readonly hashOpen: boolean;
  readonly hashQuery: string;
  readonly onSelectHash: (agent: AddressableAgent) => void;
  readonly onCloseHash: () => void;
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
  readonly agents?: ChatBoxPopupsAgents | undefined;
  readonly userMentions: ChatBoxPopupsUserMentions;
  readonly slash: ChatBoxState['slash'];
  readonly skill: ChatBoxPopupsSkill;
}

/** The agents whose name holds the "#" query (its leading "#" dropped). */
export function matchingAgents(agents: readonly AddressableAgent[], query: string): AddressableAgent[] {
  const search = (query.startsWith('#') ? query.slice(1) : query).toLowerCase();
  return agents.filter((agent) => agent.name.toLowerCase().includes(search));
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
  /** ADR-0050 7c: the conversation's agents "@" and "#" can address, and the "#" pick. */
  readonly agents?: readonly AddressableAgent[] | undefined;
  readonly onSelectHashAgent?: ((agent: AddressableAgent) => void) | undefined;
}): ChatBoxPopupsProps {
  const { state, onChangeParticipant, existingParticipants, projectId, onSelectUser, onSelectTool, onAddParticipant } = input;
  const agents = input.agents ?? [];
  // "#" lists the conversation's agents while one matches; otherwise the project picker attaches a new one.
  const hashAgents = state.keyDown.isProcessingSymbols ? matchingAgents(agents, state.keyDown.query) : [];
  return {
    agents: {
      agents: hashAgents,
      hashOpen: hashAgents.length > 0,
      hashQuery: state.keyDown.query,
      onSelectHash: (agent) => { input.onSelectHashAgent?.(agent); },
      onCloseHash: state.keyDown.stopProcessingSymbols,
    },
    hashPicker: {
      isOpen: state.keyDown.isProcessingSymbols && onAddParticipant !== undefined && hashAgents.length === 0,
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
      hasOtherUsers: state.hasOtherUsers || agents.length > 0,
      // Agents first, then people ("Everyone" only when there are other people).
      users: [...agentCandidates(agents), ...(state.hasOtherUsers ? state.users : [])],
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

export function ChatBoxPopups({ recommendations, hashPicker, agents, userMentions, slash, skill }: ChatBoxPopupsProps): ReactNode {
  return (
    <>
      {agents?.hashOpen === true && (
        <Box sx={{ mb: 1 }}>
          <UserMentionList
            users={agentCandidates(agents.agents)}
            query={agents.hashQuery}
            onSelectUser={(candidate) => { const agent = agentOfCandidate(candidate); if (agent) agents.onSelectHash(agent); }}
            onClose={agents.onCloseHash}
            title={t('widgets.chatBox.agentMentions.title', 'Agents in this conversation')}
            testId="chat-agent-mention-list"
          />
        </Box>
      )}
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
            testId="chat-user-mention-list"
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
            tools={skillMenuTools(skill.filteredItems)}
            toolkitName="Skill"
            onSelectTool={skill.onSelectTool}
            highlightedIndex={skill.highlightedIndex}
          />
        </Box>
      )}
    </>
  );
}

/**
 * The "~" menu's rows. With project skills in play the rows carry a section
 * ("Agent skills", then "Project skills", the second only for skills the agent
 * version does not carry); without any, the list is flat as it always was.
 */
function skillMenuTools(items: ChatBoxPopupsSkill['filteredItems']): MentionTool[] {
  const sectioned = items.some((item) => item.isProject);
  return items.map((item) => ({
    name: item.name,
    ...optField('description', item.description),
    ...(sectioned
      ? { group: item.isProject ? t('widgets.chatBox.projectSkills', 'Project skills') : t('widgets.chatBox.agentSkills', 'Agent skills') }
      : {}),
  }));
}
