/**
 * The team editor's draft and the rules it shares with the server
 * (`internal/domain/agentteams` Normalize): a name, at most 16 agents, no
 * agent twice, and a lead that is one of the members or none.
 *
 * The floor policy is not a separate control: a team with a lead is `lead`,
 * one without is `addressed`, the two policies of the conversation floor.
 */
import { defaultRunVersionId } from '@/entities/version';
import type { AgentTeam, AgentTeamMember, AgentTeamWrite, ApplicationVersionSummary } from '@/shared/api/generated/model';

export const MAX_TEAM_MEMBERS = 16;
export const MAX_TEAM_NAME = 128;
const MAX_TEAM_DESCRIPTION = 2000;

export interface TeamDraft {
  readonly name: string;
  readonly description: string;
  readonly members: readonly AgentTeamMember[];
  /** The lead member's 1-based position; 0 for no lead. */
  readonly lead: number;
}

export type TeamDraftProblem = 'name' | 'nameTooLong' | 'descriptionTooLong' | 'members' | 'tooManyMembers';

export function draftOf(team: AgentTeam | undefined): TeamDraft {
  if (team === undefined) return { name: '', description: '', members: [], lead: 0 };
  return {
    name: team.name,
    description: team.description,
    members: team.members.map((member) => ({ ...member })),
    lead: team.floor_policy === 'lead' ? (team.lead_position ?? 0) : 0,
  };
}

export function writeOf(draft: TeamDraft): AgentTeamWrite {
  const lead = draft.lead > 0 && draft.lead <= draft.members.length ? draft.lead : 0;
  return {
    name: draft.name.trim(),
    description: draft.description.trim(),
    floor_policy: lead > 0 ? 'lead' : 'addressed',
    lead_position: lead > 0 ? lead : null,
    members: draft.members.map((member) => ({ ...member })),
  };
}

function sameAgent(a: AgentTeamMember, b: AgentTeamMember): boolean {
  return a.application_id === b.application_id && a.project_id === b.project_id;
}

/** Adds an agent at the end; an agent already in the team, or a full team, is left as it is. */
export function withMember(draft: TeamDraft, member: AgentTeamMember): TeamDraft {
  if (draft.members.length >= MAX_TEAM_MEMBERS || draft.members.some((existing) => sameAgent(existing, member))) return draft;
  return { ...draft, members: [...draft.members, member] };
}

/** Removes the member at `index` (0-based); the lead follows its member, and is cleared with it. */
export function withoutMember(draft: TeamDraft, index: number): TeamDraft {
  if (index < 0 || index >= draft.members.length) return draft;
  const position = index + 1;
  let lead = draft.lead;
  if (lead === position) lead = 0;
  else if (lead > position) lead -= 1;
  return { ...draft, members: draft.members.filter((_, at) => at !== index), lead };
}

export function hasMember(draft: TeamDraft, applicationId: number, projectId: number): boolean {
  return draft.members.some((member) => member.application_id === applicationId && member.project_id === projectId);
}

/** The first rule the draft breaks, or `undefined`. */
export function draftProblem(draft: TeamDraft): TeamDraftProblem | undefined {
  const name = draft.name.trim();
  if (name === '') return 'name';
  if (Array.from(name).length > MAX_TEAM_NAME) return 'nameTooLong';
  if (Array.from(draft.description.trim()).length > MAX_TEAM_DESCRIPTION) return 'descriptionTooLong';
  if (draft.members.length === 0) return 'members';
  if (draft.members.length > MAX_TEAM_MEMBERS) return 'tooManyMembers';
  return undefined;
}

/**
 * The version a new member runs, by the one rule (issue 1941): the version the
 * server flags `is_default` (derived from the agent's `meta.default_version_id`),
 * else its `base`, else its first. The rule lives here so the dialog states none.
 */
export function defaultVersionOf(versions: readonly ApplicationVersionSummary[]): number | undefined {
  return defaultRunVersionId(versions, versions.find((version) => version.is_default === true)?.id);
}
