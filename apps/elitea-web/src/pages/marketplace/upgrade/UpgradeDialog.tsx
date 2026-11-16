/**
 * Upgrade to... (ADR-0051 decision 4). A person picks any version of the
 * listing, up, down or across, and reads what the change is before it happens:
 * the diff as a change list, the release notes, the rating, what the version
 * adds, and the toolkits it still needs. A low tier upgrade is one click; above
 * low tier it goes to an approver as a change request and the agent stays on its
 * pinned version until then. Every sentence is text, so a screen reader hears
 * what a colour would say.
 */
import { type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import LinearProgress from '@mui/material/LinearProgress';
import Typography from '@mui/material/Typography';

import type { MarketplaceUpgradePlan } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';
import { ChangeList } from '@/shared/ui/ChangeList';

import { BindingsStep, VersionStep } from '../hire/HireSteps';
import { useBindingTargets } from '../hire/useBindingTargets';

import { upgradeErrorMessage, wideningText } from './upgradeModel';
import { useUpgradeFlow, type UpgradeFlow } from './useUpgradeFlow';

export interface UpgradeDialogProps {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly projectId: string | undefined;
  readonly applicationId: number;
  readonly listingId: string;
  /** The version the agent is pinned to: it is not offered. */
  readonly pinnedVersionId: string;
  /** The version to start on, usually the newest. */
  readonly initialVersionId?: string | undefined;
}

function directionText(plan: MarketplaceUpgradePlan): string {
  const version = plan.target_version.name;
  if (plan.direction === 'down') return t('marketplace.upgrade.direction.down', 'Go back to {{version}}', { version });
  if (plan.direction === 'up') return t('marketplace.upgrade.direction.up', 'Upgrade to {{version}}', { version });
  return t('marketplace.upgrade.direction.across', 'Switch to {{version}}', { version });
}

function ratingText(plan: MarketplaceUpgradePlan): string {
  const { rating_avg: average, rating_count: count } = plan.feedback;
  if (average === null || average === undefined || count === 0) {
    return t('marketplace.upgrade.noRating', 'No ratings yet.');
  }
  return t('marketplace.upgrade.rating', 'Rated {{average}} out of 5 by {{count}} people.', {
    average: average.toFixed(1),
    count,
  });
}

function Widening({ plan }: { readonly plan: MarketplaceUpgradePlan }): ReactNode {
  if (plan.widening.length === 0) return null;
  return (
    <Alert severity="warning" data-testid="upgrade-widening">
      <Typography variant="bodySmall" component="p">
        {t('marketplace.upgrade.widening.title', 'This version asks for more than the one your agent runs on:')}
      </Typography>
      <Box component="ul" sx={{ margin: 0, paddingLeft: '1.25rem' }}>
        {plan.widening.map((item) => (
          <li key={`${item.kind}:${item.detail}`}>{wideningText(item)}</li>
        ))}
      </Box>
    </Alert>
  );
}

function PlanBody({ plan }: { readonly plan: MarketplaceUpgradePlan }): ReactNode {
  const state = plan.target_version.state;
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1.5 }} data-testid="upgrade-plan">
      <Typography variant="labelMedium" component="h3">
        {directionText(plan)}
      </Typography>
      {state !== 'available' && state !== 'current' && (
        <Alert severity="warning">
          {t('marketplace.upgrade.stateWarning', 'This version is {{state}}.', { state })}
          {plan.target_version.state_reason !== '' && ` ${plan.target_version.state_reason}`}
        </Alert>
      )}
      <Typography variant="bodySmall" sx={{ whiteSpace: 'pre-wrap' }}>
        {plan.target_version.release_notes !== ''
          ? plan.target_version.release_notes
          : t('marketplace.versions.noNotes', 'No release notes.')}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {ratingText(plan)}
      </Typography>
      <Widening plan={plan} />
      <Alert severity="info" data-testid="upgrade-approval-note">
        {plan.needs_approval
          ? t(
              'marketplace.upgrade.needsApproval',
              'This upgrade needs a person to approve it. Your agent keeps its current version until then.',
            )
          : t('marketplace.upgrade.oneClick', 'This upgrade needs no approval. It applies as soon as you confirm.')}
      </Alert>
      <Typography variant="labelMedium" component="h3">
        {t('marketplace.upgrade.changes', 'What changes')}
      </Typography>
      <ChangeList changes={plan.changes} />
    </Box>
  );
}

function Outcome({ flow }: { readonly flow: UpgradeFlow }): ReactNode {
  if (flow.result !== undefined) {
    return (
      <Alert severity="success" data-testid="upgrade-result">
        {flow.result.applied
          ? t('marketplace.upgrade.applied', 'The agent was upgraded.')
          : t('marketplace.upgrade.sent', 'Sent for approval. The agent stays on its version until a person approves.')}
      </Alert>
    );
  }
  if (!flow.upgrade.isError) return null;
  return (
    <Alert severity="error" data-testid="upgrade-error">
      {upgradeErrorMessage(flow.upgrade.error)}
    </Alert>
  );
}

function Body({
  flow,
  targets,
}: {
  readonly flow: UpgradeFlow;
  readonly targets: ReturnType<typeof useBindingTargets>;
}): ReactNode {
  const planned = flow.plan.data;
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2, minWidth: { xs: 0, sm: 480 } }}>
      {flow.listing.isPending ? (
        <LinearProgress />
      ) : (
        <VersionStep versions={flow.versions} value={flow.versionId} onChange={flow.choose} />
      )}
      {flow.plan.isPending && flow.versionId !== '' && <LinearProgress />}
      {flow.plan.isError && (
        <Alert severity="error" data-testid="upgrade-plan-error">
          {t('marketplace.upgrade.planFailed', 'The plan could not be read. Choose another version or try again.')}
        </Alert>
      )}
      {planned !== undefined && <PlanBody plan={planned} />}
      {planned !== undefined && planned.new_requirements.length > 0 && (
        <BindingsStep
          requirements={{ ...planned.requirements, items: planned.new_requirements }}
          choices={flow.choices}
          optionsFor={targets.optionsFor}
          onChoose={flow.bind}
        />
      )}
      <Outcome flow={flow} />
    </Box>
  );
}

export function UpgradeDialog({
  open,
  onClose,
  projectId,
  applicationId,
  listingId,
  pinnedVersionId,
  initialVersionId,
}: UpgradeDialogProps): ReactNode {
  const targets = useBindingTargets(projectId, open);
  const flow = useUpgradeFlow({ open, projectId, applicationId, listingId, pinnedVersionId, initialVersionId });
  const needsApproval = flow.plan.data?.needs_approval === true;
  return (
    <BaseModal
      open={open}
      onClose={onClose}
      onConfirm={() => !flow.upgrade.isPending && !flow.blocked && flow.upgrade.mutate()}
      title={t('marketplace.upgrade.title', 'Upgrade to...')}
      data-testid="upgrade-dialog"
      actions={{
        confirmText: needsApproval
          ? t('marketplace.upgrade.request', 'Request upgrade')
          : t('marketplace.upgrade.confirm', 'Upgrade'),
        confirming: flow.upgrade.isPending || flow.blocked,
        cancelText:
          flow.result === undefined ? t('shared.ui.baseModal.cancel', 'Cancel') : t('marketplace.upgrade.close', 'Close'),
      }}
      content={<Body flow={flow} targets={targets} />}
    />
  );
}
