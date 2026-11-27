/**
 * A team's listing page (ADR-0051 amendment, issue 3137): what the team is,
 * its members (each one its own listing, at the version the team pins), the
 * union of what they need, the versions, and **Hire** (Install or Fork) for
 * the whole team.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import LinearProgress from '@mui/material/LinearProgress';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { Link } from '@tanstack/react-router';

import type { MarketplaceListingDetail, MarketplaceTeamDetail, MarketplaceTeamMember } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { usePageCrumbs } from '@/shared/ui/page-chrome';
import { useSelectedProjectId } from '@/pages/skills/lib/useSelectedProjectId';

import { ListingFeedback } from '../feedback/ListingFeedback';
import { useMarketplaceListing } from '../useMarketplaceListings';

import { HireButton } from './HireButton';
import { ListingLoadError } from './ListingLoadError';
import { ListingNotFound } from './ListingNotFound';
import { ListingRequirements } from './ListingRequirements';
import { DelistedBanner, ListingAdminBar } from './ListingSections';
import { ListingBlock, ListingShell } from './ListingShell';
import { ListingVersions } from './ListingVersions';

function floorText(team: MarketplaceTeamDetail): string {
  const lead = team.members.find((member) => member.position === team.lead_position);
  if (team.floor_policy === 'lead' && lead !== undefined) {
    return t('marketplace.team.floorLead', '{{name}} leads the conversation.', { name: lead.name });
  }
  return t('marketplace.team.floorAddressed', 'Each agent answers when it is addressed.');
}

function TeamMemberRow({ member, isLead }: { readonly member: MarketplaceTeamMember; readonly isLead: boolean }): ReactNode {
  return (
    <Box component="li" sx={memberSx} data-testid={`marketplace-team-member-${member.position}`}>
      {member.icon_url !== null && <Box component="img" src={member.icon_url} alt="" sx={avatarSx} />}
      <Box sx={{ flex: 1, minWidth: 0 }}>
        <Link to="/marketplace/$kind/$listingId" params={{ kind: 'agents', listingId: member.listing_id }}>
          {member.name}
        </Link>
        {member.description !== '' && (
          <Typography variant="bodySmall" color="text.secondary">
            {member.description}
          </Typography>
        )}
      </Box>
      {member.version_name !== '' && <Chip size="small" variant="outlined" label={member.version_name} />}
      {isLead && <Chip size="small" color="primary" label={t('marketplace.team.lead', 'Lead')} />}
      {!member.hireable && (
        <Chip size="small" color="error" label={t('marketplace.team.memberUnavailable', 'Not hireable')} />
      )}
    </Box>
  );
}

function TeamMembers({ team }: { readonly team: MarketplaceTeamDetail }): ReactNode {
  return (
    <>
      <Typography variant="bodySmall" color="text.secondary">
        {floorText(team)}
      </Typography>
      <Box component="ul" sx={membersSx} data-testid="marketplace-team-members">
        {team.members.map((member) => (
          <TeamMemberRow
            key={member.position}
            member={member}
            isLead={team.floor_policy === 'lead' && team.lead_position === member.position}
          />
        ))}
      </Box>
    </>
  );
}

function TeamListingView({
  detail,
  projectId,
}: {
  readonly detail: MarketplaceListingDetail;
  readonly projectId: string | undefined;
}): ReactNode {
  const { listing, team } = detail;
  const current = detail.versions.find((version) => version.id === listing.current_version?.id);
  return (
    <ListingShell
      name={listing.name}
      iconUrl={listing.icon_url ?? undefined}
      version={listing.current_version?.name}
      actions={<HireButton projectId={projectId} detail={detail} />}
    >
      <DelistedBanner detail={detail} />
      <ListingAdminBar projectId={projectId} detail={detail} />
      <ListingBlock id="overview" title={t('marketplace.listing.overview', 'Overview')}>
        <Typography variant="bodySmall">
          {listing.description || t('marketplace.listing.noDescription', 'No description available.')}
        </Typography>
      </ListingBlock>
      {team !== undefined && (
        <ListingBlock id="members" title={t('marketplace.team.members', 'Members')}>
          <TeamMembers team={team} />
        </ListingBlock>
      )}
      {current !== undefined && (
        <ListingBlock id="requirements" title={t('marketplace.listing.requirements', 'Requirements')}>
          <ListingRequirements requirements={current.requirements} />
        </ListingBlock>
      )}
      <ListingBlock id="versions" title={t('marketplace.listing.versions', 'Versions')}>
        <ListingVersions
          projectId={projectId}
          listingId={listing.id}
          versions={detail.versions}
          canManage={detail.can_manage}
          canModerate={detail.can_moderate}
        />
      </ListingBlock>
      <ListingBlock id="feedback" title={t('marketplace.listing.feedback', 'Feedback')}>
        <ListingFeedback detail={detail} projectId={projectId} />
      </ListingBlock>
    </ListingShell>
  );
}

export function TeamListing({ listingId }: { readonly listingId: string }): ReactNode {
  const projectId = useSelectedProjectId();
  const { detail, isPending, isNotFound, isError, refetch } = useMarketplaceListing(projectId, listingId);
  usePageCrumbs(detail === undefined ? [] : [{ label: detail.listing.name }]);

  if (isPending) return <LinearProgress />;
  if (isError) return <ListingLoadError onRetry={refetch} />;
  if (detail === undefined || isNotFound) return <ListingNotFound />;
  return <TeamListingView detail={detail} projectId={projectId} />;
}

const membersSx: SxProps<Theme> = { margin: 0, padding: 0, listStyle: 'none', display: 'flex', flexDirection: 'column', gap: '0.5rem' };

const memberSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.75rem' };

const avatarSx: SxProps<Theme> = (theme: Theme) => ({
  width: '2rem',
  height: '2rem',
  borderRadius: theme.vars.shape.radiusMd,
});
