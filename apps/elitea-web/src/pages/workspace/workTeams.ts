/**
 * A saved team (ADR-0050 decision 8) as the runner of a Local work thread (#3053).
 *
 * Picking a team in the workspace's agent picker makes a new thread that holds what Teams → Start writes for a chat:
 * every member on the version the team pinned, and under policy `lead` the lead as `meta.floor`. The composer's agent
 * is the lead (or the first member under `addressed`); the other members are the thread's companions. Every member
 * runs on this computer with the folder, so a team is offered only when each member is one of the bound project's
 * agents and none is a pipeline (a pipeline never runs on this computer). Any other team is listed, disabled, with the
 * reason.
 */
import { FLOOR_POLICY } from '@elitea/client-core/conversation';

import type { AgentTeam, Application } from '@/shared/api/generated/model';

/** One member of a team thread, on the version the team pinned. */
interface WorkTeamMember {
  applicationId: number;
  versionId: number;
  name: string;
}

/** A team that can run a Local work thread. */
export interface WorkTeam {
  id: number;
  name: string;
  /** `lead`: the lead answers a message that names no agent (the thread's `meta.floor`). */
  lead: boolean;
  /** The composer's agent: the lead, or the first member under `addressed`. */
  primary: WorkTeamMember;
  /** The other members, in the team's order. */
  others: WorkTeamMember[];
}

/** Why a team cannot run here. */
export type TeamUnavailable = 'other_project' | 'not_listed' | 'pipeline' | 'no_members';

/** A team as the picker lists it: runnable, or why not. */
export type TeamChoice = { id: number; name: string; team: WorkTeam } | { id: number; name: string; unavailable: TeamUnavailable };

/**
 * The picker's row for `team` in the thread's bound project: runnable when every member is one of the project's agents
 * (`agents`) and none is a pipeline (`pipelineIds`).
 */
export function teamChoice(team: AgentTeam, projectId: number, agents: readonly Application[], pipelineIds: ReadonlySet<string>): TeamChoice {
  const members = team.members;
  if (members.length === 0) return { id: team.id, name: team.name, unavailable: 'no_members' };
  const known = new Set(agents.map((agent) => agent.id));
  if (members.some((member) => member.project_id !== projectId)) return { id: team.id, name: team.name, unavailable: 'other_project' };
  // An agent of this project the picker's list does not hold (gone, or past the list's first page): it cannot be told
  // apart from a pipeline, nor picked.
  if (members.some((member) => !known.has(String(member.application_id)))) return { id: team.id, name: team.name, unavailable: 'not_listed' };
  if (members.some((member) => pipelineIds.has(String(member.application_id)))) return { id: team.id, name: team.name, unavailable: 'pipeline' };
  const named = members.map((member): WorkTeamMember => {
    const agent = agents.find((a) => a.id === String(member.application_id));
    return { applicationId: member.application_id, versionId: member.version_id, name: member.name.trim() === '' ? (agent?.name ?? '') : member.name };
  });
  const lead = team.floor_policy === FLOOR_POLICY.Lead && team.lead_position !== null && team.lead_position !== undefined ? team.lead_position : 0;
  const index = lead >= 1 && lead <= named.length ? lead - 1 : 0;
  const primary = named[index] as WorkTeamMember;
  return { id: team.id, name: team.name, team: { id: team.id, name: team.name, lead: lead >= 1 && lead <= named.length, primary, others: named.filter((_, i) => i !== index) } };
}
