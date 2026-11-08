/**
 * The words of multi-agent turn-taking (ADR-0050 decisions 7c to 7f, issue #1612) on mobile:
 * the target chip, the lead, side talk, system lines and the addressee refusals. The app has
 * no translation layer yet; every string of this feature lives here so one can replace it.
 */
export const TURN_TAKING_COPY = {
  /** The target chip when nothing decides who answers yet (issue #1624). */
  chooseAgent: 'Choose an agent',
  chooseAgentHint: 'Chooses which agent answers',
  /** Said when Send is pressed before an agent is chosen; the picker opens with it. */
  chooseAgentFirst: 'Choose which agent this message is for.',
  sendTo: 'Send to',
  to: (name: string) => `To: ${name}`,
  lead: 'Lead',
  leadSuffix: (name: string) => `${name} (lead)`,
  makeLead: 'Make lead',
  clearLead: 'Clear lead',
  makeLeadLabel: (name: string) => `Make ${name} the lead`,
  clearLeadLabel: (name: string) => `Stop ${name} being the lead`,
  leadFailed: "Couldn't change the lead. Try again.",
  mentionStripLabel: 'People and agents',
  agentMentionSubtitle: 'Agent in this conversation',
  sideConversation: (count: number) => `Side conversation (${count})`,
  sideConversationHint: (open: boolean) => (open ? 'Hides the agents’ side conversation' : 'Shows the agents’ side conversation'),
  /** 422 `addressee_not_in_conversation` (Main). */
  addresseeGone: 'One of the agents you mentioned is no longer in this conversation. Remove the mention and send again.',
  addresseeGoneQueued: 'Not sent: an agent this message mentions is no longer in the conversation.',
  tooManyAddressees: 'This message mentions too many agents.',
} as const;
