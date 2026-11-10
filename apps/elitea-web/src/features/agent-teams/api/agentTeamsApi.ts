/**
 * Saved teams (ADR-0050 decision 8, issue 1620), over the generated fetchers
 * of the team routes the app calls (the single-team GET has no caller yet).
 *
 * The generated fetchers resolve the TRANSPORT envelope `{data, status,
 * headers}`, never the body: each call here unwraps it once (#132).
 */
import {
  createAgentTeam,
  createAgentTeamFromConversation,
  deleteAgentTeam,
  listAgentTeams,
  startAgentTeamConversation,
  updateAgentTeam,
} from '@/shared/api/generated/chat/chat';
import { getApplication } from '@/shared/api/generated/applications/applications';
import type {
  AgentTeam,
  AgentTeamFromConversation,
  AgentTeamList,
  AgentTeamStart,
  AgentTeamWrite,
  ApplicationDetail,
  CreatedConversation,
  ListAgentTeamsParams,
} from '@/shared/api/generated/model';

function bodyOf<T>(envelope: unknown): T {
  return (envelope as { readonly data: T }).data;
}

/* manifest: agentTeams.list */
export async function fetchAgentTeams(projectId: string, params: ListAgentTeamsParams): Promise<AgentTeamList> {
  return bodyOf<AgentTeamList>(await listAgentTeams(projectId, params));
}

/* manifest: agentTeams.create */
export async function saveAgentTeam(projectId: string, team: AgentTeamWrite): Promise<AgentTeam> {
  return bodyOf<AgentTeam>(await createAgentTeam(projectId, team));
}

/* manifest: agentTeams.update */
export async function replaceAgentTeam(projectId: string, teamId: number, team: AgentTeamWrite): Promise<AgentTeam> {
  return bodyOf<AgentTeam>(await updateAgentTeam(projectId, teamId, team));
}

/* manifest: agentTeams.delete */
export async function removeAgentTeam(projectId: string, teamId: number): Promise<void> {
  await deleteAgentTeam(projectId, teamId);
}

/* manifest: agentTeams.fromConversation */
export async function saveConversationAsTeam(projectId: string, conversationId: string, body: AgentTeamFromConversation): Promise<AgentTeam> {
  return bodyOf<AgentTeam>(await createAgentTeamFromConversation(projectId, conversationId, body));
}

/* manifest: agentTeams.start */
export async function startTeamConversation(projectId: string, teamId: number, body: AgentTeamStart): Promise<CreatedConversation> {
  return bodyOf<CreatedConversation>(await startAgentTeamConversation(projectId, teamId, body));
}

/* manifest: applications.getApplication */
export async function fetchAgentVersions(projectId: string, applicationId: number): Promise<ApplicationDetail> {
  return bodyOf<ApplicationDetail>(await getApplication(projectId, applicationId));
}
