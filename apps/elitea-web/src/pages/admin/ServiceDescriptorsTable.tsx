/**
 * The descriptor listing for the admin Service Descriptors page (unit A14).
 *
 * Reference: `frontends/admin_ui/frontend/src/pages/ServiceDescriptorsPage/
 * ServiceDescriptorsTable.jsx` (read-only). A rewrite, not a copy — that
 * component is MUI 7 over a bespoke `GridTable` plus a `useResponsiveColumns`
 * hook reading `window.innerWidth` once at render; here the columns are MUI X
 * DataGrid `flex` definitions, matching `AdminSecretsTable` and
 * `AdminProjectsTable`.
 *
 * ## Why this exists when the endpoint always refuses
 *
 * Because the alternative is worse. `ServiceDescriptors.tsx` renders the SERVER's
 * refusal rather than a hardcoded sentence, precisely so the explanation cannot
 * outlive its truth — and that property is only real if a 200 with rows produces
 * a listing. Without this component, a future stub answering 200 would render a
 * blank page and nobody would find out. With it, the rows appear and the
 * unavailability notice does not.
 *
 * It is about sixty lines and it is exercised: `ServiceDescriptors.test.tsx`
 * serves rows and asserts they render, with the notice absent.
 *
 * ## The delete action is still absent; ACTIVATION is not
 *
 * The reference puts a delete icon on every row, behind a `window.confirm`,
 * calling `DELETE /elitea_core/register_descriptor/{project_id}`. That verb
 * REVOKES on this platform — terminal, with a recorded revoker — and no page
 * control issues it. The rule this unit follows is "implement the write for
 * real, or render it unavailable — never a no-op control".
 *
 * The actions column added by migration 0109 follows the same rule in the other
 * direction: Activate and Deactivate are real writes against real routes, and
 * each appears only on the rows where the server would accept it. An inactive
 * row gets Activate; an active row gets Deactivate; a revoked or unregistered
 * row gets neither, because there is nothing the operator can do to it here and
 * a disabled button would imply there was.
 *
 * ACTIVATE CARRIES THE ROW'S OWN DIGEST. The dialog sends
 * `published_manifest_digest` as `expected_digest`, so the request asserts what
 * the operator was looking at when they decided. A digest re-read at click time
 * would assert nothing — it would agree with whatever the provider had just
 * published, which is the case the compare-and-swap exists to catch. A row with
 * no digest therefore offers no Activate: there is nothing to assert.
 */
import { memo, useMemo } from 'react';

import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Stack from '@mui/material/Stack';
import type { GridColDef, GridRenderCellParams } from '@mui/x-data-grid';
import { DataGrid } from '@mui/x-data-grid';

import { t } from '@/shared/i18n';

import type { AdminServiceDescriptor } from './api/adminServiceDescriptorsApi';

export interface AdminServiceDescriptorsTableProps {
  readonly descriptors: readonly AdminServiceDescriptor[];
  /**
   * Asked to activate one row. Optional: without both handlers the actions
   * column is not rendered at all, rather than rendered inert.
   */
  readonly onActivate?: (descriptor: AdminServiceDescriptor) => void;
  readonly onDeactivate?: (descriptor: AdminServiceDescriptor) => void;
}

interface DescriptorGridRow extends AdminServiceDescriptor {
  readonly id: string;
}

/**
 * The row key is the descriptor's own identity — the legacy runtime keyed the
 * stored row on exactly this triple (`json.dumps({project_id, provider_name,
 * service_location_url}, sort_keys=True)`), so one provider registered at two
 * URLs, or in two projects, is two rows and must not collapse into one.
 */
function rowKey(descriptor: AdminServiceDescriptor): string {
  return JSON.stringify([
    descriptor.project_id,
    descriptor.provider_name,
    descriptor.service_location_url,
  ]);
}

/**
 * The THREE presentations of the `healthy` column, each pairing its colour with
 * its label. `label` is a function because `t` must be called at render time,
 * not at module load, or a locale switch would not reach it.
 *
 * The third one is the reason this is a lookup and not a ternary. The server
 * answers `null` for "no health projection is fresh enough to say", which is a
 * different statement from "down" — the legacy runtime could not make it, and
 * reading that `null` as `false` here would put the lie back on the screen
 * after the server stopped telling it. A two-state column FORCES the server to
 * choose, which is why the API type is `boolean | null` and this map has three
 * entries.
 */
const HEALTH_CHIPS = {
  yes: {
    color: 'success',
    label: () => t('pages.admin.serviceDescriptors.healthy.yes', 'Yes'),
  },
  no: {
    color: 'error',
    label: () => t('pages.admin.serviceDescriptors.healthy.no', 'No'),
  },
  unknown: {
    color: 'default',
    label: () => t('pages.admin.serviceDescriptors.healthy.unknown', 'Unknown'),
  },
} as const;

/**
 * `undefined` joins `null` in the unknown bucket. A row that omits the field
 * entirely says exactly as little as one that sends null, and only `true` and
 * `false` are claims.
 */
function healthChip(value: boolean | null | undefined) {
  if (value === true) return HEALTH_CHIPS.yes;
  if (value === false) return HEALTH_CHIPS.no;
  return HEALTH_CHIPS.unknown;
}

/**
 * The admission states, and the one column that makes a REVOKE visible.
 *
 * The listing is driven by every origin ever registered, and DELETE revokes
 * rather than deleting — an admission that was once in force is a fact about
 * what this deployment ran. So a revoked provider stays in the table. Without
 * this column it stays there looking exactly like a live one, and an operator
 * who revokes it, reloads, and sees no change concludes the revoke failed.
 *
 * `unregistered` is not an error: it is an origin with no admitted revision
 * yet, which is a real state and the one an operator is usually looking for.
 */
const STATUS_CHIPS: Record<string, { readonly color: 'success' | 'warning' | 'error' | 'default'; readonly label: () => string }> = {
  active: {
    color: 'success',
    label: () => t('pages.admin.serviceDescriptors.status.active', 'Active'),
  },
  inactive: {
    color: 'warning',
    label: () => t('pages.admin.serviceDescriptors.status.inactive', 'Inactive'),
  },
  revoked: {
    color: 'error',
    label: () => t('pages.admin.serviceDescriptors.status.revoked', 'Revoked'),
  },
  unregistered: {
    color: 'default',
    label: () => t('pages.admin.serviceDescriptors.status.unregistered', 'Not registered'),
  },
};

/**
 * A state this build does not know is shown VERBATIM rather than mapped to a
 * default. The admission plane can gain a state before this page does, and
 * rendering an unfamiliar one as "Not registered" would be a confident wrong
 * answer where the raw word is a correct one.
 */
function statusChip(value: unknown): { color: 'success' | 'warning' | 'error' | 'default'; label: string } {
  const key = typeof value === 'string' && value !== '' ? value : 'unregistered';
  const known = STATUS_CHIPS[key];
  return known ? { color: known.color, label: known.label() } : { color: 'default', label: key };
}

export const AdminServiceDescriptorsTable = memo(function AdminServiceDescriptorsTable({
  descriptors,
  onActivate,
  onDeactivate,
}: AdminServiceDescriptorsTableProps) {
  const rows = useMemo<DescriptorGridRow[]>(
    () => descriptors.map((descriptor) => ({ ...descriptor, id: rowKey(descriptor) })),
    [descriptors],
  );

  const columns = useMemo<GridColDef<DescriptorGridRow>[]>(
    () => [
      {
        field: 'project_id',
        headerName: t('pages.admin.serviceDescriptors.column.project', 'Project ID'),
        width: 120,
      },
      {
        field: 'provider_name',
        headerName: t('pages.admin.serviceDescriptors.column.provider', 'Provider'),
        flex: 1,
        minWidth: 160,
      },
      {
        field: 'service_location_url',
        headerName: t('pages.admin.serviceDescriptors.column.url', 'Service URL'),
        flex: 2,
        minWidth: 240,
      },
      {
        field: 'status',
        headerName: t('pages.admin.serviceDescriptors.column.status', 'Admission'),
        width: 140,
        sortable: true,
        renderCell: (params: GridRenderCellParams<DescriptorGridRow, string | undefined>) => {
          const status = statusChip(params.value);
          return (
            <Chip
              size="small"
              variant="outlined"
              color={status.color}
              label={status.label}
              title={params.row.reason === undefined || params.row.reason === '' ? undefined : params.row.reason}
            />
          );
        },
      },
      {
        field: 'healthy',
        headerName: t('pages.admin.serviceDescriptors.column.healthy', 'Healthy'),
        width: 120,
        sortable: true,
        // The colour and the label come from ONE selection, not two ternaries.
        // Two would be two places to flip, and a green chip reading "No" is a
        // defect no reasonable assertion can catch: R-T6 forbids asserting on
        // MUI's internal class names, so only the label is observable in a
        // test. Selecting the pair together makes the pair impossible to
        // disagree with itself.
        renderCell: (params: GridRenderCellParams<DescriptorGridRow, boolean | null>) => {
          const health = healthChip(params.value);
          return <Chip size="small" variant="outlined" color={health.color} label={health.label()} />;
        },
      },
      ...(onActivate && onDeactivate
        ? [
            {
              field: 'actions',
              headerName: t('pages.admin.serviceDescriptors.column.actions', 'Actions'),
              width: 180,
              sortable: false,
              filterable: false,
              renderCell: (params: GridRenderCellParams<DescriptorGridRow>) => {
                const row = params.row;
                if (row.status === 'active') {
                  return (
                    <Stack direction="row" sx={{ alignItems: 'center', height: '100%' }}>
                      <Button
                        size="small"
                        color="warning"
                        onClick={() => onDeactivate(row)}
                      >
                        {t('pages.admin.serviceDescriptors.action.deactivate', 'Deactivate')}
                      </Button>
                    </Stack>
                  );
                }
                // Only `inactive` can be activated, and only with a digest to
                // assert. `revoked` is terminal and `unregistered` has no
                // revision — offering a control on either would be a button
                // whose only outcome is a refusal.
                if (row.status !== 'inactive' || !row.published_manifest_digest) return null;
                return (
                  <Stack direction="row" sx={{ alignItems: 'center', height: '100%' }}>
                    <Button size="small" onClick={() => onActivate(row)}>
                      {t('pages.admin.serviceDescriptors.action.activate', 'Activate')}
                    </Button>
                  </Stack>
                );
              },
            } satisfies GridColDef<DescriptorGridRow>,
          ]
        : []),
    ],
    [onActivate, onDeactivate],
  );

  return (
    <DataGrid
      rows={rows}
      columns={columns}
      disableRowSelectionOnClick
      autoHeight
      hideFooterSelectedRowCount
      aria-label={t('pages.admin.serviceDescriptors.tableLabel', 'Registered service descriptors')}
      localeText={{
        noRowsLabel: t('pages.admin.serviceDescriptors.empty', 'No service descriptors are registered.'),
      }}
    />
  );
});
