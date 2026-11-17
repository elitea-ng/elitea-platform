/**
 * Conversation skills (ADR-0050 amendment 1, skills epic 2169): the skills pinned
 * to a conversation. A pin is a skill, or a Marketplace skill listing version, that
 * applies to every run of the conversation whichever agent or model answers. It is
 * an attachment, never a participant, and it carries no skill text.
 *
 * The one place the pin routes are called from, so the chat panel, the skill
 * pages and the composer read and change the same list.
 */
export type { ConversationSkillPin } from '@/shared/api/generated/model';
export {
  CONVERSATION_SKILL_LIMIT,
  conversationSkillKeys,
  useConversationSkillMutations,
  useConversationSkills,
} from './model/useConversationSkills';
