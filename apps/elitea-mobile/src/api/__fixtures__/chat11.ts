/**
 * Wire fixtures for the client contract 1.1 chat reads, shaped after the
 * schemas of the pinned contract (contract/elitea-v2.yaml @ 06a490427):
 * ApplicationDetail, ConversationParticipant, ParticipantCandidatesPage.
 * Ids are numeric strings where the schema says so. Restated as data.
 */

/** getApplication: an agent whose current version enables `attachments` and `ask_user`. */
export function applicationDetail(id = 31, versionId = 310, internalTools: string[] = ['attachments', 'ask_user']) {
  return {
    id: String(id),
    name: 'Approver',
    description: 'Asks before it acts.',
    icon: '',
    owner_id: '9',
    created_at: '2026-10-05T09:00:00Z',
    versions: [{ id: String(versionId), name: 'base', status: 'draft', agent_type: 'react', created_at: '2026-10-05T09:00:00Z', is_default: true }],
    version_details: {
      id: String(versionId),
      application_id: String(id),
      name: 'base',
      status: 'draft',
      agent_type: 'react',
      meta: { internal_tools: internalTools, step_limit: 25 },
      tools: [],
    },
  };
}

/** One addConversationParticipants row for an agent. */
export function agentParticipantRow(rowId: number, agentId: number, versionId: number, projectId = 7) {
  return {
    id: rowId,
    entity_name: 'application',
    entity_meta: { id: agentId, project_id: projectId },
    entity_settings: { version_id: versionId },
    meta: { name: 'Approver' },
  };
}

/** listParticipantCandidates page. */
export function candidatesPage(rows: { user_id: number; name: string; email?: string; already_participant: boolean }[], next: string | null = null) {
  return { rows, has_more: next !== null, next_cursor: next };
}
