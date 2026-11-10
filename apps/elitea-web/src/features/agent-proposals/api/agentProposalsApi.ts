/**
 * Agent proposals and Copy to project (ADR-0050 decisions 5a and 1, issue
 * 1615), over the generated fetchers.
 *
 * The generated fetchers resolve the TRANSPORT envelope `{data, status,
 * headers}`, never the body: each call here unwraps it once, so no caller can
 * read a field of the envelope as if it were the answer (#132).
 *
 * Create is two calls on purpose. The agent is made by the ordinary create
 * route, under the caller's own permission and with the create route's own
 * defaults (tier low, memory on); only then does the proposal record which
 * agent it became and seed the memory the person kept. Nothing is saved as
 * an agent before the person presses Create.
 */
import {
  copyAgentMemory,
  createApplication,
  dismissAgentProposal,
  exportApplication,
  forkAgent,
  getAgentProposal,
  listAgentMemory,
  listAgentProposals,
  markAgentProposalCreated,
} from '@/shared/api/generated/applications/applications';
import type {
  AgentMemoryList,
  AgentProposal,
  AgentProposalList,
  ApplicationCreateRequest,
  ApplicationCreatedResponse,
  ForkRequest,
  ListAgentProposalsParams,
  MarkAgentProposalCreated200,
} from '@/shared/api/generated/model';

function bodyOf<T>(envelope: unknown): T {
  return (envelope as { readonly data: T }).data;
}

/* manifest: agentProposals.list */
export async function fetchAgentProposals(projectId: string, params: ListAgentProposalsParams): Promise<AgentProposalList> {
  return bodyOf<AgentProposalList>(await listAgentProposals(projectId, params));
}

/* manifest: agentProposals.get */
export async function fetchAgentProposal(projectId: string, proposalId: number): Promise<AgentProposal> {
  return bodyOf<AgentProposal>(await getAgentProposal(projectId, proposalId));
}

/* manifest: agentProposals.dismiss */
export async function dismissProposal(projectId: string, proposalId: number): Promise<AgentProposal> {
  return bodyOf<AgentProposal>(await dismissAgentProposal(projectId, proposalId));
}

/* manifest: applications.createApplication */
export async function createProposedAgent(projectId: string, request: ApplicationCreateRequest): Promise<ApplicationCreatedResponse> {
  return bodyOf<ApplicationCreatedResponse>(await createApplication(projectId, request));
}

/* manifest: agentProposals.markCreated */
export async function markProposalCreated(
  projectId: string,
  proposalId: number,
  applicationId: number,
  memorySeeds: readonly number[],
): Promise<MarkAgentProposalCreated200> {
  return bodyOf<MarkAgentProposalCreated200>(
    await markAgentProposalCreated(projectId, proposalId, { application_id: applicationId, memory_seeds: [...memorySeeds] }),
  );
}

/* manifest: agentMemory.list */
export async function fetchCopyableMemory(projectId: string, applicationId: number): Promise<AgentMemoryList> {
  return bodyOf<AgentMemoryList>(await listAgentMemory(projectId, applicationId, { status: 'active', limit: 200 }));
}

/**
 * Forks a personal agent into another project and answers the copy's id.
 * The fork document is READ from the source with `?fork=true` and written to
 * the target, exactly as the editor's Fork does.
 */
/* manifest: applications.exportApplication, applications.forkAgent */
export async function forkAgentInto(sourceProjectId: string, applicationId: number, targetProjectId: string): Promise<number | undefined> {
  const document = bodyOf<{ readonly applications?: readonly unknown[]; readonly skills?: readonly unknown[] }>(
    await exportApplication(sourceProjectId, applicationId, { fork: true }),
  );
  const request = {
    applications: document.applications ?? [],
    ...(document.skills === undefined ? {} : { skills: document.skills }),
  } as ForkRequest;
  const outcome = bodyOf<{ readonly result?: { readonly agents?: readonly { readonly id?: unknown }[] } }>(await forkAgent(targetProjectId, request));
  const id = Number(outcome.result?.agents?.[0]?.id);
  return Number.isInteger(id) && id > 0 ? id : undefined;
}

/* manifest: agentProposals.copyMemory */
export async function copyMemoryInto(
  targetProjectId: string,
  targetApplicationId: number,
  sourceProjectId: number,
  sourceApplicationId: number,
  entryIds: readonly number[],
): Promise<number> {
  return bodyOf<{ readonly copied: number }>(
    await copyAgentMemory(targetProjectId, targetApplicationId, {
      source_project_id: sourceProjectId,
      source_application_id: sourceApplicationId,
      entry_ids: [...entryIds],
    }),
  ).copied;
}
