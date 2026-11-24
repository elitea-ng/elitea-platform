import { importTeam } from '@/shared/api/generated/applications/applications';
import type { TeamImportRequest } from '@/shared/api/generated/model';

import type { EntityExportDocument, ImportOutcome } from './lifecycleApi';

/** The team a team import creates, beside the agents (issue 3010). */
export interface TeamImportTeamBody {
  readonly name: string;
  readonly description: string;
  readonly floor_policy?: string | undefined;
  readonly lead?: string | undefined;
  readonly members: readonly string[];
}

/**
 * Posts a team zip's document to the team import: every agent and the team
 * in one transaction, all or nothing. A refusal (a taken name, a missing
 * toolkit, an entity the server could not write) arrives as an
 * `EliteaApiError` whose body carries the server's sentence; nothing of the
 * team was kept. `eliteaFetch` resolves the `{data, status}` envelope, so the
 * body is read through `.data`.
 */
export async function importTeamDocument(
  projectId: string,
  document: EntityExportDocument,
  team: TeamImportTeamBody,
): Promise<ImportOutcome> {
  const request = {
    team: {
      name: team.name,
      description: team.description,
      members: [...team.members],
      ...(team.floor_policy === undefined ? {} : { floor_policy: team.floor_policy }),
      ...(team.lead === undefined ? {} : { lead: team.lead }),
    },
    applications: document.applications ?? [],
    toolkits: document.toolkits ?? [],
    skills: document.skills ?? [],
  } as unknown as TeamImportRequest;
  const envelope = (await importTeam(projectId, request)) as unknown as { readonly data: Record<string, unknown>; readonly status: number };
  return {
    status: envelope.status,
    result: envelope.data['result'],
    errors: envelope.data['errors'],
  };
}
