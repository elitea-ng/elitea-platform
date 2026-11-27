/**
 * The team hire's rules, with no React in them (ADR-0051 amendment, issue
 * 3137): which members a hire makes and which it reuses, what connections each
 * still needs, the request, and what a refusal says.
 *
 * Install reuses an agent this project already installed from the same member
 * listing version; Fork always makes new editable agents. Every member the hire
 * makes needs its required toolkits bound, or marked "Disable for now", exactly
 * as a single agent hire does.
 */
import type {
  MarketplaceTeamDetail,
  MarketplaceTeamHireRequest,
  MarketplaceTeamMember,
} from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { t } from '@/shared/i18n';

import { bindingsOf, hireErrorMessage, unboundRequirements, type BindingChoices, type HireMode } from './hireModel';

/** The choices per member, by roster position. */
export type TeamChoices = Readonly<Record<number, BindingChoices | undefined>>;

/** Install reuses the project's installed agent of the same listing version. */
export function reusesMember(member: MarketplaceTeamMember, mode: HireMode): boolean {
  return mode === 'install' && member.hired_application_id !== null;
}

/** The members whose connections the person still has to choose, by position. */
export function membersStillUnbound(team: MarketplaceTeamDetail, mode: HireMode, choices: TeamChoices): number[] {
  return team.members
    .filter((member) => !reusesMember(member, mode))
    .filter((member) => unboundRequirements(member.requirements, choices[member.position] ?? {}).length > 0)
    .map((member) => member.position);
}

/** A member the team cannot be hired without: its listing is gone or its pinned version is not hireable. */
export function unavailableMembers(team: MarketplaceTeamDetail): MarketplaceTeamMember[] {
  return team.members.filter((member) => !member.hireable);
}

/** The team hire's request body. A reused member sends no bindings. */
export function teamHireRequest(
  listingId: string,
  team: MarketplaceTeamDetail,
  mode: HireMode,
  name: string,
  choices: TeamChoices,
): MarketplaceTeamHireRequest {
  const trimmed = name.trim();
  return {
    listing_id: listingId,
    version_id: team.version_id,
    mode,
    ...(trimmed === '' ? {} : { name: trimmed }),
    members: team.members
      .filter((member) => !reusesMember(member, mode))
      .map((member) => ({
        position: member.position,
        bindings: bindingsOf(member.requirements, choices[member.position] ?? {}),
      })),
  };
}

interface TeamRefusalBody {
  readonly error?: unknown;
  readonly member?: unknown;
}

function refusalBody(error: unknown): TeamRefusalBody | undefined {
  if (!(error instanceof EliteaApiError) || error.failure.kind !== 'http') return undefined;
  return (error.failure.body as TeamRefusalBody | null) ?? undefined;
}

/** One sentence for a failed team hire: the agent hire's words, with the member it is about. */
export function teamHireErrorMessage(error: unknown): string {
  const body = refusalBody(error);
  if (body?.error === 'team_name_taken') {
    return t('marketplace.teamHire.error.nameTaken', 'A team with this name already exists in this project. Choose another name.');
  }
  const message = hireErrorMessage(error);
  if (typeof body?.member === 'string' && body.member !== '') {
    return t('marketplace.teamHire.error.member', '{{member}}: {{message}}', { member: body.member, message });
  }
  return message;
}
