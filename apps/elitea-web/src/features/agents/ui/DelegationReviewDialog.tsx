import type { ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import type { DelegationClosureNode, DelegationItem, DelegationPreview } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { BaseModal } from '@/shared/ui/BaseModal';

import { groupItems, groupWithheldReason, isWithheld, itemKey, itemsToApprove, toolLabel } from '../lib/delegationReview';
import type { ToolkitGroup } from '../lib/delegationReview';

/**
 * The review of what a sub-agent, agent tool or pipeline child brings
 * (ADR-0044 amendment 1, A1.2 rule 2; epic 2156 story S3). It opens when an
 * editor attaches an agent or pipeline X to Y, and again from the row's
 * "Review delegation" action.
 *
 * It lists what X will be able to use (its toolkits and the tools selected on
 * each, the credential reference each names by name, the data classes it
 * declares and the egress hosts its toolkits reach) and, as information only,
 * the callees X reaches through approved grants. A toolkit the approver cannot
 * access is shown disabled with the reason, and only the rest is approved: the
 * server applies its own access reading to the same list, so a client that
 * ticks too much approves no more than the person holds.
 *
 * It never approves by itself: `onConfirm` carries the items to keep (`undefined`
 * for everything the person may approve) and the caller makes the calls.
 */
export interface DelegationReviewDialogProps {
  readonly open: boolean;
  /** The agent whose reach is reviewed. */
  readonly calleeName: string;
  readonly preview: DelegationPreview;
  /** `toolkit:<id>` to the toolkit's name. */
  readonly names: ReadonlyMap<string, string>;
  /** The primary action's label; `undefined` makes the dialog read-only (nothing the person can decide). */
  readonly confirmLabel?: string | undefined;
  /** A line above the list, for a state the list does not say (awaiting a change request, needs re-approval). */
  readonly notice?: string | undefined;
  readonly busy?: boolean | undefined;
  readonly error?: string | undefined;
  readonly onCancel: () => void;
  readonly onConfirm: (items: readonly DelegationItem[] | undefined) => void;
}

const listSx = (theme: Theme) => ({ display: 'flex', flexDirection: 'column', gap: theme.spacing(2), m: 0, p: 0, listStyle: 'none' }) as const;

function ToolkitRow({ group, reason, ticked, onToggle }: { readonly group: ToolkitGroup; readonly reason: string | undefined; readonly ticked: boolean; readonly onToggle: (key: string) => void }): ReactNode {
  const withheld = reason !== undefined;
  const tools = group.tools.map(toolLabel).join(', ');
  const credentials = group.credentials.map((item) => item.name).join(', ');
  return (
    <Box
      component="li"
      data-testid="delegation-toolkit"
      sx={{ display: 'flex', flexDirection: 'column' }}
    >
      <FormControlLabel
        disabled={withheld}
        control={
          <BaseCheckbox
            checked={ticked && !withheld}
            disabled={withheld}
            onChange={() => onToggle(group.key)}
            aria-label={group.label}
          />
        }
        label={<Typography variant="labelMedium">{group.label}</Typography>}
      />
      {tools === '' ? null : (
        <Typography
          variant="bodySmall"
          color="text.secondary"
          sx={{ pl: 4 }}
        >
          {t('agents.delegation.tools', 'Tools: {{tools}}', { tools })}
        </Typography>
      )}
      {credentials === '' ? null : (
        <Typography
          variant="bodySmall"
          color="text.secondary"
          sx={{ pl: 4 }}
        >
          {t('agents.delegation.credentials', 'Credential: {{credentials}}', { credentials })}
        </Typography>
      )}
      {withheld ? (
        <Typography
          data-testid="delegation-withheld-reason"
          variant="bodySmall"
          color="text.secondary"
          sx={{ pl: 4 }}
        >
          {reason}
        </Typography>
      ) : null}
    </Box>
  );
}

function ClosureList({ nodes }: { readonly nodes: readonly DelegationClosureNode[] }): ReactNode {
  const below = nodes.filter((node) => node.depth > 0);
  if (below.length === 0) return null;
  return (
    <Box
      component="section"
      aria-label={t('agents.delegation.closureTitle', 'Also reachable through this agent')}
      sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}
    >
      <Typography variant="subtitle">{t('agents.delegation.closureTitle', 'Also reachable through this agent')}</Typography>
      <Typography
        variant="bodySmall"
        color="text.secondary"
      >
        {t('agents.delegation.closureNote', 'Information only. Each of these has its own approval, given by an editor of the agent that attached it.')}
      </Typography>
      {below.map((node) => (
        <Typography
          key={node.application_id}
          variant="bodySmall"
          data-testid="delegation-closure-node"
        >
          {t('agents.delegation.closureNode', '{{name}}: {{items}}', {
            name: node.name,
            items: node.items
              .filter((item) => item.kind === 'toolkit')
              .map((item) => item.name)
              .join(', '),
          })}
        </Typography>
      ))}
    </Box>
  );
}

function InfoLine({ label, items }: { readonly label: string; readonly items: readonly DelegationItem[] }): ReactNode {
  if (items.length === 0) return null;
  return (
    <Typography variant="bodySmall">
      {label} {items.map((item) => item.name).join(', ')}
    </Typography>
  );
}

export function DelegationReviewDialog({ open, calleeName, preview, names, confirmLabel, notice, busy = false, error, onCancel, onConfirm }: DelegationReviewDialogProps): ReactNode {
  const [unticked, setUnticked] = useState<ReadonlySet<string>>(new Set());
  const grouped = useMemo(() => groupItems(preview.items, names), [preview.items, names]);
  const withheld = useMemo(() => new Set(preview.withheld.map(itemKey)), [preview.withheld]);
  const approvableToolkits = grouped.toolkits.filter((group) => !isWithheld(group, withheld));
  const everythingWithheld = grouped.toolkits.length > 0 && approvableToolkits.length === 0 && grouped.egressHosts.length === 0 && grouped.dataClasses.length === 0;
  const nothingTicked = approvableToolkits.every((group) => unticked.has(group.key)) && grouped.dataClasses.length === 0 && grouped.egressHosts.length === 0;

  const onToggle = useCallback((key: string) => {
    setUnticked((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }, []);

  const confirm = useCallback(() => onConfirm(itemsToApprove(preview.approvable, unticked)), [onConfirm, preview.approvable, unticked]);

  return (
    <BaseModal
      open={open}
      onClose={onCancel}
      {...(confirmLabel === undefined ? {} : { onConfirm: confirm })}
      variant="simple"
      title={t('agents.delegation.title', 'What {{name}} will be able to use', { name: calleeName })}
      data-testid="delegation-review-dialog"
      actions={{
        ...(confirmLabel === undefined ? {} : { confirmText: confirmLabel }),
        cancelText: confirmLabel === undefined ? t('agents.delegation.close', 'Close') : t('agents.delegation.cancel', 'Cancel'),
        confirming: busy,
        confirmDisabled: everythingWithheld || nothingTicked,
      }}
      content={
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 3, minWidth: { sm: '30rem' } }}>
          {notice === undefined ? null : (
            <BannerMessage
              message={notice}
              variant="info"
            />
          )}
          <Typography
            variant="bodyMedium"
            color="text.secondary"
          >
            {t('agents.delegation.intro', 'When this agent is used as a tool, it can use the following even if the agent using it does not hold them. You approve only what you can access yourself.')}
          </Typography>
          <Box
            component="ul"
            aria-label={t('agents.delegation.toolkitsLabel', 'Toolkits')}
            sx={listSx}
          >
            {grouped.toolkits.map((group) => (
              <ToolkitRow
                key={group.key}
                group={group}
                reason={isWithheld(group, withheld) ? groupWithheldReason(group, withheld) : undefined}
                ticked={!unticked.has(group.key)}
                onToggle={onToggle}
              />
            ))}
          </Box>
          <InfoLine
            label={t('agents.delegation.dataClasses', 'Data classes:')}
            items={grouped.dataClasses}
          />
          <InfoLine
            label={t('agents.delegation.egressHosts', 'Hosts it can reach:')}
            items={grouped.egressHosts}
          />
          <ClosureList nodes={preview.closure} />
          {error === undefined ? null : (
            <BannerMessage
              message={error}
              variant="error"
            />
          )}
        </Box>
      }
    />
  );
}
