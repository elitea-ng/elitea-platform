/**
 * The conversation-history modes of an agent version's `meta.context`
 * (ADR-0050 decision 9) and their one set of labels (issue 1894).
 *
 * The agent editor's Conversation history field and the chat's "What the
 * agent saw" inspector both name a mode; they used to carry two copies of the
 * four labels under two key sets. Here so both slices read it without one
 * reaching into the other.
 */
import { t } from '@/shared/i18n';

/** `meta.context.history`. */
export type ContextHistoryMode = 'full' | 'since_joined' | 'addressed_only' | 'none';

export const CONTEXT_HISTORY_MODES: readonly ContextHistoryMode[] = ['full', 'since_joined', 'addressed_only', 'none'];

/** The label of a history mode; a mode this client does not know shows as itself. */
export function contextHistoryLabel(mode: string): string {
  switch (mode) {
    case 'since_joined':
      return t('features.agents.contextHistory.sinceJoined', 'Since it joined');
    case 'addressed_only':
      return t('features.agents.contextHistory.addressedOnly', 'Only messages addressed to it');
    case 'none':
      return t('features.agents.contextHistory.none', 'None (the current message only)');
    case 'full':
      return t('features.agents.contextHistory.full', 'Everything it may see');
    default:
      return mode;
  }
}
