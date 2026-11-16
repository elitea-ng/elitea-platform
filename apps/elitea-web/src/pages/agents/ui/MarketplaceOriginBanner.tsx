import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import { Link } from '@tanstack/react-router';

import { BindingsEditorDialog, UpgradeDialog, UpgradePolicyControl, UpstreamReviewDialog } from '@/pages/marketplace';

import type { MarketplaceOrigin } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

/**
 * The origin notices on top of a hired agent's editor (ADR-0051 decisions 3 and 4).
 * Every notice is text, so a screen reader hears what a colour would say.
 * An installed agent says its definition is read-only and why; a forked one
 * says only where it came from. A yanked or deprecated version, a withdrawn
 * listing and a newer version each get their own line. An installed agent
 * offers **Upgrade to...** and its upgrade policy; a fork offers **Review
 * update** when upstream has a newer version.
 */
export interface MarketplaceOriginBannerProps {
  readonly origin: MarketplaceOrigin;
  /** With both, the banner offers Edit bindings: the toolkits an installed agent uses stay a project setting. */
  readonly projectId?: string | undefined;
  readonly applicationId?: number | undefined;
}

function yankedNext(installed: boolean): string {
  return installed
    ? t('pages.agents.origin.yankedNext', 'Upgrade it to another version.')
    : t('pages.agents.origin.yankedForkNext', 'Review the upstream update when a new version is out.');
}

function OriginLine({ origin, children }: { readonly origin: MarketplaceOrigin; readonly children?: ReactNode }): ReactNode {
  const installed = origin.mode === 'installed';
  const line = installed
    ? t(
        'pages.agents.origin.installed',
        'Installed from {{listing}} {{version}}. The definition is read-only. Fork it to change it.',
        {
          listing: origin.listing_name,
          version: origin.version_name,
        },
      )
    : t('pages.agents.origin.forked', 'Forked from {{listing}} {{version}}.', {
        listing: origin.listing_name,
        version: origin.version_name,
      });
  return (
    <Alert severity="info" data-testid="marketplace-origin-line">
      {line}{' '}
      <Link to="/marketplace/$kind/$listingId" params={{ kind: 'agents', listingId: origin.listing_id }}>
        {t('pages.agents.origin.openListing', 'Open the listing')}
      </Link>
      {children}
    </Alert>
  );
}

function YankedNotice({ origin }: { readonly origin: MarketplaceOrigin }): ReactNode {
  const next = yankedNext(origin.mode === 'installed');
  const reason = origin.version_state_reason ?? '';
  return (
    <Alert severity="warning" data-testid="marketplace-origin-yanked">
      {reason !== ''
        ? t('pages.agents.origin.yankedReason', 'This version was yanked: {{reason}} Your agent keeps running. {{next}}', {
            reason,
            next,
          })
        : t('pages.agents.origin.yankedPlain', 'This version was yanked. Your agent keeps running. {{next}}', { next })}
    </Alert>
  );
}

function StateNotices({ origin }: { readonly origin: MarketplaceOrigin }): ReactNode {
  const latest = origin.latest_version;
  return (
    <>
      {origin.listing_state === 'delisted' && (
        <Alert severity="warning" data-testid="marketplace-origin-withdrawn">
          {t(
            'pages.agents.origin.withdrawn',
            'Listing withdrawn. The publisher took it down. Your agent keeps running on its copy.',
          )}
        </Alert>
      )}
      {origin.version_state === 'yanked' && <YankedNotice origin={origin} />}
      {origin.version_state === 'deprecated' && (
        <Alert severity="warning" data-testid="marketplace-origin-deprecated">
          {t('pages.agents.origin.deprecated', 'This version is deprecated. Check the listing for a newer one.')}
        </Alert>
      )}
      {latest != null && (
        <Alert severity="info" data-testid="marketplace-origin-update">
          {origin.mode === 'installed'
            ? t('pages.agents.origin.updateAvailable', 'Update available: {{version}}.', { version: latest.name })
            : t('pages.agents.origin.upstreamAvailable', 'Upstream update available: {{version}}.', {
                version: latest.name,
              })}{' '}
          <Link
            to="/marketplace/$kind/$listingId"
            params={{ kind: 'agents', listingId: origin.listing_id }}
            hash="versions"
          >
            {t('pages.agents.origin.seeVersions', 'See the versions')}
          </Link>
        </Alert>
      )}
    </>
  );
}

interface ActionProps {
  readonly origin: MarketplaceOrigin;
  readonly projectId: string;
  readonly applicationId: number;
}

/** An installed agent: Upgrade to... and its policy. */
function InstalledActions({ origin, projectId, applicationId }: ActionProps): ReactNode {
  const [upgrading, setUpgrading] = useState(false);
  return (
    <Box sx={{ display: 'flex', flexWrap: 'wrap', alignItems: 'flex-start', gap: '0.75rem' }}>
      <Button variant="outlined" size="small" onClick={() => setUpgrading(true)} data-testid="marketplace-upgrade-to">
        {t('pages.agents.origin.upgradeTo', 'Upgrade to...')}
      </Button>
      <UpgradePolicyControl origin={origin} projectId={projectId} applicationId={applicationId} />
      {upgrading && (
        <UpgradeDialog
          open
          onClose={() => setUpgrading(false)}
          projectId={projectId}
          applicationId={applicationId}
          listingId={origin.listing_id}
          pinnedVersionId={origin.version_id}
          initialVersionId={origin.latest_version?.id}
        />
      )}
    </Box>
  );
}

/** A fork with a newer upstream version: Review update, a three-way view. */
function ForkActions({ projectId, applicationId }: Omit<ActionProps, 'origin'>): ReactNode {
  const [reviewing, setReviewing] = useState(false);
  return (
    <Box sx={{ display: 'flex', gap: '0.75rem' }}>
      <Button variant="outlined" size="small" onClick={() => setReviewing(true)} data-testid="marketplace-review-update">
        {t('pages.agents.origin.reviewUpdate', 'Review update')}
      </Button>
      {reviewing && (
        <UpstreamReviewDialog open onClose={() => setReviewing(false)} projectId={projectId} applicationId={applicationId} />
      )}
    </Box>
  );
}

function OriginActions({ origin, projectId, applicationId }: ActionProps): ReactNode {
  if (origin.mode === 'installed') {
    return <InstalledActions origin={origin} projectId={projectId} applicationId={applicationId} />;
  }
  return origin.latest_version == null ? null : <ForkActions projectId={projectId} applicationId={applicationId} />;
}

function EditBindings({ projectId, applicationId }: Omit<ActionProps, 'origin'>): ReactNode {
  const [editing, setEditing] = useState(false);
  return (
    <>
      {' '}
      <Button size="small" onClick={() => setEditing(true)} data-testid="marketplace-edit-bindings">
        {t('pages.agents.origin.editBindings', 'Edit bindings')}
      </Button>
      {/* Mounted only while open: a closed editor reads nothing from the project. */}
      {editing && (
        <BindingsEditorDialog open onClose={() => setEditing(false)} projectId={projectId} applicationId={applicationId} />
      )}
    </>
  );
}

export function MarketplaceOriginBanner({ origin, projectId, applicationId }: MarketplaceOriginBannerProps): ReactNode {
  const ids = projectId !== undefined && applicationId !== undefined ? { projectId, applicationId } : undefined;
  return (
    <Box
      component="section"
      aria-label={t('pages.agents.origin.groupLabel', 'Where this agent came from')}
      data-testid="marketplace-origin-banner"
      sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}
    >
      <OriginLine origin={origin}>{ids !== undefined && <EditBindings {...ids} />}</OriginLine>
      <StateNotices origin={origin} />
      {ids !== undefined && <OriginActions origin={origin} {...ids} />}
    </Box>
  );
}
