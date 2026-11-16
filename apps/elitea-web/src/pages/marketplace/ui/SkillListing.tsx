/**
 * A skill's listing page: what it is, its tags, its instructions, its versions
 * with release notes, and **Add to an agent**, which forks the chosen version of
 * the published skill into the caller's project and attaches it to an agent
 * version (`AttachPublicSkillDialog`). A skill's Add flow has the version pick
 * (ADR-0051 decision 3): the current version by default, any version that is not
 * yanked.
 *
 * Adding forks the skill into the project, so the action needs the fork
 * permission: without it the page still reads, and says why the button is off,
 * instead of offering an attach that can only end in a 403.
 */
import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import LinearProgress from '@mui/material/LinearProgress';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { AttachPublicSkillDialog, type PublicSkillSummary } from '@/features/skills';
import type { MarketplaceListingDetail, MarketplaceVersion, PublicSkillDetail } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';
import { useGetPublicSkill } from '@/shared/api/generated/skills/skills';
import { t } from '@/shared/i18n';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { usePageCrumbs } from '@/shared/ui/page-chrome';
import { useSelectedProjectId } from '@/pages/skills/lib/useSelectedProjectId';
import { usePermissionSet } from '@/widgets/sidebar';

import { defaultVersionId, isHireable } from '../hire/hireModel';
import { useScrollToHash } from '../useScrollToHash';
import { useMarketplaceListing } from '../useMarketplaceListings';

import { ListingLoadError } from './ListingLoadError';
import { ListingNotFound } from './ListingNotFound';
import { ListingFeedback } from '../feedback/ListingFeedback';
import { DelistedBanner, ListingAdminBar, ListingUsedBy } from './ListingSections';
import { ListingBlock, ListingShell } from './ListingShell';
import { ListingVersions } from './ListingVersions';

interface SkillListingViewProps {
  readonly detail: MarketplaceListingDetail;
  readonly projectId: string | undefined;
}

/** The version pick of the Add flow: the current version by default, a yanked one disabled. */
function VersionPick({
  versions,
  value,
  onChange,
}: {
  readonly versions: readonly MarketplaceVersion[];
  readonly value: string;
  readonly onChange: (id: string) => void;
}): ReactNode {
  const label = t('marketplace.listing.skillVersion', 'Version');
  return (
    <TextField
      select
      size="small"
      label={label}
      value={value}
      onChange={(event) => onChange(event.target.value)}
      slotProps={{ htmlInput: { 'aria-label': label, 'data-testid': 'marketplace-skill-version' } }}
      sx={{ minWidth: '9rem' }}
    >
      {versions.map((version) => (
        <MenuItem key={version.id} value={version.id} disabled={!isHireable(version)}>
          {`${version.name} (${version.state})`}
        </MenuItem>
      ))}
    </TextField>
  );
}

function SkillOverview({
  detail,
  instructions,
}: {
  readonly detail: MarketplaceListingDetail;
  readonly instructions: string;
}): ReactNode {
  const { listing } = detail;
  return (
    <>
      <ListingBlock id="overview" title={t('marketplace.listing.overview', 'Overview')}>
        <Typography variant="bodyMedium">
          {listing.description || t('marketplace.listing.noDescription', 'No description available.')}
        </Typography>
        {listing.tags.length > 0 && (
          <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 0.5 }}>
            {listing.tags.map((tag) => (
              <Chip key={tag} size="small" label={tag} />
            ))}
          </Box>
        )}
      </ListingBlock>
      <ListingBlock id="instructions" title={t('marketplace.listing.instructions', 'Instructions')}>
        <Typography
          variant="bodySmall"
          component="pre"
          sx={{ margin: 0, whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}
          data-testid="marketplace-skill-instructions"
        >
          {instructions}
        </Typography>
      </ListingBlock>
    </>
  );
}

function SkillSections({ detail, projectId }: SkillListingViewProps): ReactNode {
  const { listing } = detail;
  return (
    <>
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
      {listing.hired.length > 0 && (
        <ListingBlock id="used-by" title={t('marketplace.listing.usedBy', 'Used by this project')}>
          <ListingUsedBy hired={listing.hired} kind="skills" />
        </ListingBlock>
      )}
    </>
  );
}

/** What the Add flow needs: the chosen version, whether Add is allowed, the skill row the attach dialog reads, and the skill's instructions. */
function useSkillAdd(detail: MarketplaceListingDetail, projectId: string | undefined) {
  const permissions = usePermissionSet(projectId);
  const { listing } = detail;
  const skillId = Number(listing.entity_id);
  const content = useGetPublicSkill(skillId, { query: { enabled: Number.isInteger(skillId) && skillId > 0 } });
  const skill = content.data?.status === 200 ? (unwrapBody(content.data) as PublicSkillDetail) : undefined;
  const [picked, setPicked] = useState('');
  const versionId = picked === '' ? defaultVersionId(detail.versions) : picked;
  const summary: PublicSkillSummary = {
    id: skillId,
    name: listing.name,
    description: listing.description,
    tags: listing.tags,
    versions: [],
  };
  return {
    versionId,
    setPicked,
    summary,
    instructions: skill?.version_details?.instructions ?? '',
    delisted: listing.state === 'delisted',
    canFork: permissions.has(PERMISSIONS.applications.fork),
    attachable: versionId !== '' && Number.isInteger(Number(versionId)),
  };
}

function SkillListingView({ detail, projectId }: SkillListingViewProps): ReactNode {
  const { listing } = detail;
  const add = useSkillAdd(detail, projectId);
  useScrollToHash();
  const [adding, setAdding] = useState(false);
  return (
    <>
      <ListingShell
        name={listing.name}
        iconUrl={listing.icon_url ?? undefined}
        version={listing.current_version?.name}
        actions={
          <>
            <VersionPick versions={detail.versions} value={add.versionId} onChange={add.setPicked} />
            <Button
              variant="contained"
              disabled={!add.canFork || add.delisted || !add.attachable}
              onClick={() => setAdding(true)}
              data-testid="marketplace-add-skill"
            >
              {t('marketplace.listing.addSkill', 'Add to an agent')}
            </Button>
          </>
        }
      >
        <DelistedBanner detail={detail} />
        <ListingAdminBar projectId={projectId} detail={detail} />
        {!add.canFork && !add.delisted && (
          <Alert severity="info">
            {t(
              'marketplace.listing.noFork',
              'You can read this skill, but adding it to an agent needs the fork permission in this project.',
            )}
          </Alert>
        )}
        <SkillOverview detail={detail} instructions={add.instructions} />
        <SkillSections detail={detail} projectId={projectId} />
      </ListingShell>
      <AttachPublicSkillDialog
        open={adding}
        projectId={projectId}
        skill={add.summary}
        versionId={add.attachable ? Number(add.versionId) : undefined}
        onClose={() => setAdding(false)}
      />
    </>
  );
}

export function SkillListing({ listingId }: { readonly listingId: string }): ReactNode {
  const projectId = useSelectedProjectId();
  const { detail, isPending, isNotFound, isError, refetch } = useMarketplaceListing(projectId, listingId);
  usePageCrumbs(detail === undefined ? [] : [{ label: detail.listing.name }]);

  if (isPending) return <LinearProgress />;
  if (isError) return <ListingLoadError onRetry={refetch} />;
  if (detail === undefined || isNotFound) return <ListingNotFound />;
  return <SkillListingView detail={detail} projectId={projectId} />;
}
