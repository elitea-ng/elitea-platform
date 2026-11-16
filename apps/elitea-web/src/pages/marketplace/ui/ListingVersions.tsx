/**
 * The listing page's **Versions** section (ADR-0051 decision 2): newest first,
 * each with its state as text (current, available, deprecated, yanked), the
 * publish date, a short digest, the release notes and what it requires. The
 * publisher can deprecate, yank and restore a version; a platform administrator
 * can yank. Every change asks for a confirmation with an optional reason.
 */
import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import FormControlLabel from '@mui/material/FormControlLabel';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useMutation, useQueryClient } from '@tanstack/react-query';

import { setMarketplaceVersionState } from '@/shared/api/generated/marketplace/marketplace';
import type { MarketplaceVersion } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { BaseModal } from '@/shared/ui/BaseModal';

import { invalidateMarketplace } from '../invalidate';

import { ListingRequirements } from './ListingRequirements';
import { versionActions, type VersionAction } from './versionActions';

export interface ListingVersionsProps {
  readonly projectId: string | undefined;
  readonly listingId: string;
  readonly versions: readonly MarketplaceVersion[];
  readonly canManage: boolean;
  readonly canModerate: boolean;
}

interface Pending {
  readonly version: MarketplaceVersion;
  readonly action: VersionAction;
}

function actionLabel(action: VersionAction): string {
  switch (action) {
    case 'deprecated':
      return t('marketplace.versions.deprecate', 'Deprecate');
    case 'yanked':
      return t('marketplace.versions.yank', 'Yank');
    case 'available':
      return t('marketplace.versions.restore', 'Restore');
  }
}

function actionExplanation(action: VersionAction): string {
  switch (action) {
    case 'deprecated':
      return t('marketplace.versions.deprecateHelp', 'A deprecated version can still be hired. People see a warning.');
    case 'yanked':
      return t(
        'marketplace.versions.yankHelp',
        'A yanked version cannot be hired. Agents installed from it keep running and show a warning.',
      );
    case 'available':
      return t('marketplace.versions.restoreHelp', 'The version can be hired again.');
  }
}

function VersionRow({
  version,
  actions,
  onAction,
}: {
  readonly version: MarketplaceVersion;
  readonly actions: readonly VersionAction[];
  readonly onAction: (action: VersionAction) => void;
}): ReactNode {
  return (
    <Box component="li" sx={rowSx} data-testid={`marketplace-version-${version.id}`}>
      <Box sx={headRowSx}>
        <Typography component="span" variant="labelMedium">
          {version.name}
        </Typography>
        <Chip
          size="small"
          variant="outlined"
          label={t('marketplace.versions.state', 'State: {{state}}', { state: version.state })}
          data-testid={`marketplace-version-state-${version.id}`}
        />
        <Typography component="span" variant="bodySmall" color="text.secondary">
          {new Date(version.published_at).toLocaleDateString()}
        </Typography>
        <Typography component="span" variant="bodySmall" color="text.secondary" title={version.digest}>
          {t('marketplace.versions.digest', 'Digest {{digest}}', { digest: version.digest.slice(0, 8) })}
        </Typography>
        {actions.map((action) => (
          <Button
            key={action}
            size="small"
            onClick={() => onAction(action)}
            data-testid={`marketplace-version-${action}-${version.id}`}
          >
            {actionLabel(action)}
          </Button>
        ))}
      </Box>
      {version.state === 'deprecated' && (
        <Alert severity="warning">
          {version.state_reason ||
            t('marketplace.versions.deprecatedWarning', 'The publisher deprecated this version.')}
        </Alert>
      )}
      {version.state === 'yanked' && (
        <Alert severity="error">
          {version.state_reason ||
            t('marketplace.versions.yankedWarning', 'This version was yanked. You cannot hire it.')}
        </Alert>
      )}
      {version.release_notes !== '' ? (
        <Typography variant="bodySmall" sx={{ whiteSpace: 'pre-wrap' }}>
          {version.release_notes}
        </Typography>
      ) : (
        <Typography variant="bodySmall" color="text.secondary">
          {t('marketplace.versions.noNotes', 'No release notes.')}
        </Typography>
      )}
      <ListingRequirements requirements={version.requirements} compact />
    </Box>
  );
}

export function ListingVersions({
  projectId,
  listingId,
  versions,
  canManage,
  canModerate,
}: ListingVersionsProps): ReactNode {
  const queryClient = useQueryClient();
  const [pending, setPending] = useState<Pending>();
  const [reason, setReason] = useState('');
  const [suspend, setSuspend] = useState(false);
  const change = useMutation({
    mutationFn: ({ version, action }: Pending) =>
      setMarketplaceVersionState(projectId ?? '', listingId, version.id, {
        state: action,
        ...(reason.trim() === '' ? {} : { reason: reason.trim() }),
        // Only an administrator's yank may also suspend the agents that hold the version (ADR-0041).
        ...(suspend && action === 'yanked' && canModerate ? { suspend: true } : {}),
      }),
    onSuccess: () => {
      invalidateMarketplace(queryClient);
      setPending(undefined);
    },
  });
  const ask = (version: MarketplaceVersion, action: VersionAction): void => {
    setReason('');
    setSuspend(false);
    change.reset();
    setPending({ version, action });
  };
  return (
    <>
      <Box component="ul" sx={listSx}>
        {versions.map((version) => (
          <VersionRow
            key={version.id}
            version={version}
            actions={versionActions(version, canManage, canModerate)}
            onAction={(action) => ask(version, action)}
          />
        ))}
      </Box>
      <BaseModal
        open={pending !== undefined}
        onClose={() => setPending(undefined)}
        // One request at a time: a second click or Enter while the first is in flight is ignored.
        onConfirm={() => pending !== undefined && !change.isPending && change.mutate(pending)}
        title={
          pending === undefined
            ? ''
            : t('marketplace.versions.confirmTitle', '{{action}} {{version}}?', {
                action: actionLabel(pending.action),
                version: pending.version.name,
              })
        }
        data-testid="marketplace-version-confirm"
        actions={{
          confirmText: pending === undefined ? '' : actionLabel(pending.action),
          confirming: change.isPending,
        }}
        content={
          <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2, minWidth: { xs: 0, sm: 360 } }}>
            <Typography variant="bodySmall">
              {pending === undefined ? '' : actionExplanation(pending.action)}
            </Typography>
            <TextField
              size="small"
              label={t('marketplace.versions.reason', 'Reason (optional)')}
              value={reason}
              onChange={(event) => setReason(event.target.value)}
              slotProps={{ htmlInput: { maxLength: 500 } }}
            />
            {pending?.action === 'yanked' && canModerate && (
              <FormControlLabel
                control={<BaseCheckbox size="small" checked={suspend} onChange={(event) => setSuspend(event.target.checked)} />}
                label={t(
                  'marketplace.versions.suspend',
                  'Also suspend the agents installed from this version (reason: listing_yanked)',
                )}
                data-testid="marketplace-version-suspend"
              />
            )}
            {change.isError && (
              <Alert severity="error">{t('marketplace.versions.failed', 'The change failed. Try again.')}</Alert>
            )}
          </Box>
        }
      />
    </>
  );
}

const listSx: SxProps<Theme> = {
  listStyle: 'none',
  margin: 0,
  padding: 0,
  display: 'flex',
  flexDirection: 'column',
  gap: '1rem',
};
const rowSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.5rem',
  paddingBottom: '1rem',
  borderBottom: 1,
  borderColor: 'divider',
};
const headRowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: '0.75rem' };
