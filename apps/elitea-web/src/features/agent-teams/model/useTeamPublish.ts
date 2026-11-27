/**
 * Publish on a team row (ADR-0051 amendment, issue 3137): lists the saved
 * team in the Marketplace. Every member agent must itself be listed; the
 * server refuses with `team_members_unlisted` and names them, and that refusal
 * is shown in a sentence naming the agents to publish first.
 */
import { useState } from 'react';

import { useMutation } from '@tanstack/react-query';

import { publishMarketplaceTeam } from '@/shared/api/generated/marketplace/marketplace';
import type { AgentTeam, MarketplaceTeamPublishResult } from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';

interface UnlistedBody {
  readonly error?: unknown;
  readonly members?: readonly { readonly name?: unknown }[];
}

/** The sentence for a failed publish: the unlisted members by name, or a plain failure. */
function teamPublishErrorMessage(error: unknown): string {
  const body =
    error instanceof EliteaApiError && error.failure.kind === 'http'
      ? (error.failure.body as UnlistedBody | null)
      : null;
  if (body?.error === 'team_members_unlisted') {
    const names = (body.members ?? [])
      .map((member) => (typeof member.name === 'string' ? member.name : ''))
      .filter((name) => name !== '');
    return t(
      'features.agentTeams.publishUnlisted',
      'Publish these agents to the Marketplace first: {{names}}.',
      { names: names.join(', ') },
    );
  }
  return t('features.agentTeams.publishFailed', 'The team was not published. Try again.');
}

export interface PublishedTeam {
  readonly teamId: number;
  readonly listingId: string;
  readonly versionName: string;
}

export function useTeamPublish(projectId: string) {
  const [published, setPublished] = useState<PublishedTeam>();
  const publish = useMutation<MarketplaceTeamPublishResult, unknown, AgentTeam>({
    mutationFn: async (team) =>
      unwrapBody(await publishMarketplaceTeam(projectId, team.id, {})) as MarketplaceTeamPublishResult,
    onMutate: () => setPublished(undefined),
    onSuccess: (result, team) =>
      setPublished({ teamId: team.id, listingId: result.listing_id, versionName: result.version_name }),
  });
  return {
    publish: (team: AgentTeam) => publish.mutate(team),
    publishingId: publish.isPending ? publish.variables?.id : undefined,
    error: publish.isError ? teamPublishErrorMessage(publish.error) : undefined,
    published,
  };
}
