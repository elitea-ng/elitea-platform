import type { ReactNode } from 'react';

import Button from '@mui/material/Button';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';

import type { PermissionTrimmingItem, PermissionTrimmingItemKind } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

/**
 * The permission trimming report as a table (ADR-0044 decision 5, #1501):
 * each granted tool, toolkit, credential use grant and egress allowlist entry
 * against its use in the window's step records. An unused item is a removal
 * candidate with a one-click Remove; the server performs it through the same
 * mutation path as the editor or the settings page, and audits it.
 *
 * Shared by the agent's Governance › Permission trimming tab (one agent) and
 * Admin › Permission trimming (every project, with project and agent
 * columns).
 */
export interface PermissionTrimmingTableProps {
  readonly items: readonly PermissionTrimmingItem[];
  readonly showAgent: boolean;
  readonly removingKey: string | undefined;
  readonly onRemove: (item: PermissionTrimmingItem) => void;
  /** A link to the owning agent; plain text without it. */
  readonly agentHref?: (item: PermissionTrimmingItem) => string | undefined;
}

export function trimmingItemKey(item: PermissionTrimmingItem): string {
  return `${item.project_id}:${item.application_id}:${item.kind}:${item.key}`;
}

function trimmingKindLabel(kind: PermissionTrimmingItemKind): string {
  switch (kind) {
    case 'tool':
      return t('features.agentLifecycle.trimming.kind.tool', 'Tool');
    case 'toolkit':
      return t('features.agentLifecycle.trimming.kind.toolkit', 'Toolkit');
    case 'credential_grant':
      return t('features.agentLifecycle.trimming.kind.credentialGrant', 'Credential grant');
    case 'egress_host':
      return t('features.agentLifecycle.trimming.kind.egressHost', 'Egress host');
  }
}

function egressLabel(item: PermissionTrimmingItem): string {
  const host = item.host ?? item.key;
  return item.allowlist_level === 'platform'
    ? t('features.agentLifecycle.trimming.platformHost', '{{host}} (platform)', { host })
    : host;
}

function grantLabel(item: PermissionTrimmingItem): string {
  return `${item.toolkit_name ?? ''} › ${item.credential_reference_id ?? item.grant_id ?? item.key}`;
}

function itemLabel(item: PermissionTrimmingItem): string {
  switch (item.kind) {
    case 'tool':
      return `${item.toolkit_name ?? ''} › ${item.tool ?? ''}`;
    case 'toolkit':
      return item.toolkit_name ?? item.key;
    case 'credential_grant':
      return grantLabel(item);
    case 'egress_host':
      return egressLabel(item);
  }
}

function notRemovableLabel(reason: string | undefined): string {
  switch (reason ?? '') {
    case 'last_selected_tool':
      return t('features.agentLifecycle.trimming.lastTool', 'The last selected tool: remove the toolkit instead.');
    case 'published_version':
      return t('features.agentLifecycle.trimming.published', 'Published version: edit the draft.');
    default:
      return reason ?? '';
  }
}

function usage(item: PermissionTrimmingItem): string {
  if (item.too_new === true) {
    return t('features.agentLifecycle.trimming.tooNew', 'Too new to judge: granted for less than the window');
  }
  if (!item.used) return t('features.agentLifecycle.trimming.unused', 'Not used in the window');
  const last = item.last_used_at ? new Date(item.last_used_at).toLocaleDateString() : '—';
  return t('features.agentLifecycle.trimming.used', '{{uses}} uses, last {{last}}', { uses: item.uses, last });
}

export function PermissionTrimmingTable({
  items,
  showAgent,
  removingKey,
  onRemove,
  agentHref,
}: PermissionTrimmingTableProps): ReactNode {
  if (items.length === 0) {
    return (
      <Typography variant="bodyMedium" data-testid="permission-trimming-empty">
        {t(
          'features.agentLifecycle.trimming.empty',
          'No removal candidates: every granted permission was used in the window.',
        )}
      </Typography>
    );
  }
  return (
    <Table size="small" data-testid="permission-trimming-table">
      <TableHead>
        <TableRow>
          {showAgent && <TableCell>{t('features.agentLifecycle.trimming.project', 'Project')}</TableCell>}
          {showAgent && <TableCell>{t('features.agentLifecycle.trimming.agent', 'Agent')}</TableCell>}
          <TableCell>{t('features.agentLifecycle.trimming.kindColumn', 'Kind')}</TableCell>
          <TableCell>{t('features.agentLifecycle.trimming.item', 'Permission')}</TableCell>
          <TableCell>{t('features.agentLifecycle.trimming.usage', 'Use')}</TableCell>
          <TableCell />
        </TableRow>
      </TableHead>
      <TableBody>
        {items.map((item) => (
          <TrimmingRow
            key={trimmingItemKey(item)}
            item={item}
            showAgent={showAgent}
            removingKey={removingKey}
            onRemove={onRemove}
            href={agentHref?.(item)}
          />
        ))}
      </TableBody>
    </Table>
  );
}

interface TrimmingRowProps {
  readonly item: PermissionTrimmingItem;
  readonly showAgent: boolean;
  readonly removingKey: string | undefined;
  readonly onRemove: (item: PermissionTrimmingItem) => void;
  readonly href: string | undefined;
}

function TrimmingRow({ item, showAgent, removingKey, onRemove, href }: TrimmingRowProps): ReactNode {
  return (
    <TableRow data-testid={`permission-trimming-row-${item.kind}-${item.key}`}>
      {showAgent && <TableCell>{item.project_id === 0 ? '—' : item.project_id}</TableCell>}
      {showAgent && (
        <TableCell>
          <AgentCell item={item} href={href} />
        </TableCell>
      )}
      <TableCell>{trimmingKindLabel(item.kind)}</TableCell>
      <TableCell>
        {itemLabel(item)}
        <UnusedScopes item={item} />
      </TableCell>
      <TableCell data-testid="permission-trimming-usage">{usage(item)}</TableCell>
      <TableCell align="right">
        <RowAction item={item} removingKey={removingKey} onRemove={onRemove} />
      </TableCell>
    </TableRow>
  );
}

function AgentCell({
  item,
  href,
}: {
  readonly item: PermissionTrimmingItem;
  readonly href: string | undefined;
}): ReactNode {
  if (item.application_id === 0) return '—';
  const name = item.application_name ?? `#${item.application_id}`;
  return href === undefined ? name : <a href={href}>{name}</a>;
}

function UnusedScopes({ item }: { readonly item: PermissionTrimmingItem }): ReactNode {
  const scopes = item.unused_scopes ?? [];
  if (item.kind !== 'credential_grant' || scopes.length === 0) return null;
  return (
    <Typography variant="bodySmall" component="div" color="text.secondary">
      {t('features.agentLifecycle.trimming.unusedScopes', 'Unused scopes: {{scopes}}', { scopes: scopes.join(', ') })}
    </Typography>
  );
}

function RowAction({
  item,
  removingKey,
  onRemove,
}: {
  readonly item: PermissionTrimmingItem;
  readonly removingKey: string | undefined;
  readonly onRemove: (item: PermissionTrimmingItem) => void;
}): ReactNode {
  if (!item.candidate) return null;
  if (!item.removable) {
    return (
      <Typography variant="bodySmall" color="text.secondary">
        {notRemovableLabel(item.not_removable_reason)}
      </Typography>
    );
  }
  return (
    <Button
      size="small"
      color="error"
      disabled={removingKey !== undefined}
      onClick={() => onRemove(item)}
      data-testid={`permission-trimming-remove-${item.kind}-${item.key}`}
    >
      {removingKey === trimmingItemKey(item)
        ? t('features.agentLifecycle.trimming.removing', 'Removing…')
        : t('features.agentLifecycle.trimming.remove', 'Remove')}
    </Button>
  );
}
