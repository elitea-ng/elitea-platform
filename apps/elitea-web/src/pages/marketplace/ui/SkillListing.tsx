/**
 * A skill's listing page: what it is, its tags, its instructions, and **Add to
 * an agent**, which forks the published skill into the caller's project and
 * attaches it to an agent version (`AttachPublicSkillDialog`).
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
import Typography from '@mui/material/Typography';

import { AttachPublicSkillDialog, publishedVersionIdOf, type PublicSkillSummary } from '@/features/skills';
import type { PublicSkillDetail } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';
import { useGetPublicSkill } from '@/shared/api/generated/skills/skills';
import { t } from '@/shared/i18n';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { usePageCrumbs } from '@/shared/ui/page-chrome';
import { useSelectedProjectId } from '@/pages/skills/lib/useSelectedProjectId';
import { usePermissionSet } from '@/widgets/sidebar';

import { ListingBlock, ListingShell } from './ListingShell';
import { ListingNotFound } from './ListingNotFound';

/** The catalogue-row shape the attach dialog and the version lookup read. */
function summaryOf(skill: PublicSkillDetail): PublicSkillSummary {
  return { id: skill.id, name: skill.name, description: skill.description, tags: skill.version_details?.tags ?? [], versions: skill.versions ?? [] };
}

function SkillListingView({ skill }: { readonly skill: PublicSkillDetail }): ReactNode {
  const projectId = useSelectedProjectId();
  const permissions = usePermissionSet(projectId);
  const [adding, setAdding] = useState(false);
  const summary = summaryOf(skill);
  const versionId = publishedVersionIdOf(summary);
  const canAdd = permissions.has(PERMISSIONS.applications.fork);
  const tags = summary.tags ?? [];
  return (
    <>
      <ListingShell
        name={skill.name ?? ''}
        iconUrl={undefined}
        version={skill.version_details?.name}
        actions={
          <Button variant="contained" disabled={!canAdd || versionId === undefined} onClick={() => setAdding(true)} data-testid="marketplace-add-skill">
            {t('marketplace.listing.addSkill', 'Add to an agent')}
          </Button>
        }
      >
        {!canAdd && <Alert severity="info">{t('marketplace.listing.noFork', 'You can read this skill, but adding it to an agent needs the fork permission in this project.')}</Alert>}
        <ListingBlock id="overview" title={t('marketplace.listing.overview', 'Overview')}>
          <Typography variant="bodyMedium">{skill.description || t('marketplace.listing.noDescription', 'No description available.')}</Typography>
          {tags.length > 0 && (
            <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 0.5 }}>
              {tags.map((tag) => (
                <Chip key={tag} size="small" label={tag} />
              ))}
            </Box>
          )}
        </ListingBlock>
        <ListingBlock id="instructions" title={t('marketplace.listing.instructions', 'Instructions')}>
          <Typography variant="bodySmall" component="pre" sx={{ margin: 0, whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }} data-testid="marketplace-skill-instructions">
            {skill.version_details?.instructions ?? ''}
          </Typography>
        </ListingBlock>
      </ListingShell>
      <AttachPublicSkillDialog open={adding} projectId={projectId} skill={summary} versionId={versionId} onClose={() => setAdding(false)} />
    </>
  );
}

export function SkillListing({ listingId }: { readonly listingId: string }): ReactNode {
  const skillId = Number(listingId);
  const query = useGetPublicSkill(skillId, { query: { enabled: Number.isInteger(skillId) && skillId > 0 } });
  const skill = query.data?.status === 200 ? (unwrapBody(query.data) as PublicSkillDetail) : undefined;
  usePageCrumbs(skill?.name === undefined ? [] : [{ label: skill.name }]);

  if (query.isPending && query.fetchStatus !== 'idle') return <LinearProgress />;
  if (skill === undefined) return <ListingNotFound />;
  return <SkillListingView skill={skill} />;
}
