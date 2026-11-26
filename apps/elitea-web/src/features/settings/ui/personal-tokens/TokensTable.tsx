/**
 * Personal access tokens data table — shows name, masked value, expiration
 * status, and delete action per row.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/ui/personal-tokes/
 * TokensTable.jsx`.
 *
 * Key deviations from the baseline:
 *  - No Redux (no sidebar/collapsed state tracking)
 *  - No tour IDs
 *  - On the shared `GridTable` (`shared/ui/grid-table`, the port of the
 *    old-app entity; ADR-0062 decision 4, D35); its `emptyMessage="No
 *    tokens"` shows when a search matches nothing (Warning #8)
 *  - Uses RTK Query hooks from `entities/token/api/tokenApi`
 *  - Uses selectors from `entities/token/model/selectors` for masking/sorting
 *  - Delete confirmation uses `DeleteEntityModal` from `shared/ui`
 *  - Uses `OpenEyeIcon` from `shared/ui/icons`
 *  - Accepts `search` prop for filtering tokens by name
 *  - Gates its own token-list fetch on the user's `personal_project_id`
 *    (TanStack Router context, same seam
 *    `features/toolkits/sharepoint/lib/hooks/
 *    useResolvedSharepointConfig.hooks.ts` reads — no shared `shared/`
 *    primitive for it exists yet, so this is a local copy per that file's
 *    own documented convention), NOT the currently-selected project
 *    (Warning #11) — personal tokens are not project-scoped
 *    (`/auth/token/` takes no project param)
 */
import { memo, useMemo } from 'react';

import { useRouteContext } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import { useTheme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import {
  GridTable,
  GridTableBody,
  GridTableHeader,
  GridTableRow,
  SortComparators,
  useResponsiveColumns,
  useTableSort,
  type GridTableColumn,
} from '@/shared/ui/grid-table';
import {
  maskedTokenValue,
  tokenProjectKey,
  useDeleteTokenMutation,
  useListTokensQuery,
} from '@/entities/token';
import type { PersonalAccessToken } from '@/entities/token';
import { ExpiryCell, ActionsCell } from './TokenRow';
import { TokenAdminAccessCell } from './TokenAdminAccessCell';
import { tokensTableStyles } from './TokensTable.styles';

/**
 * `personal_project_id` from the TanStack Router root context's
 * `auth.getUser()` (`src/app/router-context.ts`'s `AuthUser.
 * personal_project_id` — outside this cluster's file scope, read
 * structurally rather than imported, per `no-upward-from-features`).
 */
interface PersonalProjectIdContext {
  readonly auth?: {
    readonly getUser?: () => { readonly personal_project_id?: string } | undefined;
  };
}

function isPersonalProjectIdContext(value: unknown): value is PersonalProjectIdContext {
  return typeof value === 'object' && value !== null;
}

/** Pure extraction, mirrors `useResolvedSharepointConfig.hooks.ts`'s `selectPersonalProjectId`. */
function selectPersonalProjectId(context: unknown): string | undefined {
  if (!isPersonalProjectIdContext(context)) return undefined;
  return context.auth?.getUser?.()?.personal_project_id;
}

/* ── column definitions ────────────────────────────────────────────────── */

type ColumnId = 'name' | 'token' | 'project' | 'administration' | 'expires' | 'actions';

interface ColumnDef {
  id: ColumnId;
  label: string;
  sortable: boolean;
  /** The grid track; the old app's `TOKENS_COLUMNS` widths. */
  width: string;
}

const COLUMNS: ColumnDef[] = [
  { id: 'name', label: t('entities.token.table.name', 'Token name'), sortable: true, width: '1.5fr' },
  { id: 'token', label: t('entities.token.table.tokenValue', 'Token value'), sortable: false, width: '1fr' },
  { id: 'project', label: t('entities.token.table.project', 'Project'), sortable: false, width: '1fr' },
  { id: 'expires', label: t('entities.token.table.expiration', 'Expiration'), sortable: true, width: '0.8fr' },
  { id: 'actions', label: t('entities.token.table.actions', 'Actions'), sortable: false, width: '9.375rem' },
];

/**
 * The administration column (ADR-0059 R1.3b) appears only when a token has
 * administration access: for everyone else the table stays as it was.
 */
const ADMIN_COLUMN: ColumnDef = { id: 'administration', label: t('entities.token.table.adminAccess', 'Administration'), sortable: false, width: '1.5fr' };
const COLUMNS_WITH_ADMIN: ColumnDef[] = [...COLUMNS.slice(0, 3), ADMIN_COLUMN, ...COLUMNS.slice(3)];

function columnsFor(tokens: readonly PersonalAccessToken[]): ColumnDef[] {
  return tokens.some((token) => (token.admin_access ?? 'none') !== 'none') ? COLUMNS_WITH_ADMIN : COLUMNS;
}

/* ── project binding cell (spec-llm-project-scope §4) ──────────────────── */

/**
 * What this key bills. An UNBOUND key reads as a word, never as an empty
 * cell — blank cannot be told apart from "the name did not load".
 *
 * A bound key whose project is not in `projectNames` still says which project
 * it is, by id: the name map comes from the caller's own project list, and a
 * key can outlive the caller's membership of the project it bills — exactly
 * the case a user most needs to see.
 */
function projectCellLabel(
  row: PersonalAccessToken,
  projectNames: ReadonlyMap<string, string> | undefined,
): string {
  const key = tokenProjectKey(row);
  if (key === null) return t('entities.token.table.projectNone', 'Not bound');
  /*
   * i18n trap: the bundle value carries `{{id}}`, and a bundle placeholder
   * BEATS the call-site fallback — so this passes an interpolation option
   * and must never be written as a template literal.
   */
  return projectNames?.get(key) ?? t('entities.token.table.projectId', 'Project {{id}}', { id: key });
}

/* ── main table ────────────────────────────────────────────────────────── */

export interface TokensTableProps {
  /** Search query to filter token names. */
  search?: string;
  /** Whether the "preview settings" button should appear in actions. */
  showPreview?: boolean;
  /** Callback when a user clicks "Preview settings" on a token. */
  onPreviewToken?: (token: PersonalAccessToken) => void;
  /**
   * Project id -> project name, for the binding column. Supplied by the route
   * (`routes/_shell/settings/tokens.tsx`), which reads the app's existing
   * projects query; this component cannot fetch it itself, because the query
   * lives in `widgets/` and `features/` may not import upward (R-L1). Absent
   * or incomplete is fine — the cell falls back to the project id.
   */
  projectNames?: ReadonlyMap<string, string>;
}

/** `expires` sorts as a date; a token that never expires sorts first, as before. */
const SORT_COMPARATORS = { expires: SortComparators.date };

export const TokensTable = memo(function TokensTable({
  search = '',
  showPreview = false,
  onPreviewToken,
  projectNames,
}: TokensTableProps) {
  const routeContext: unknown = useRouteContext({ strict: false });
  const personalProjectId = selectPersonalProjectId(routeContext);
  const { data: tokens = [], isFetching } = useListTokensQuery({
    enabled: !!personalProjectId,
  });
  const deleteMutation = useDeleteTokenMutation();
  const theme = useTheme();
  const styles = tokensTableStyles(theme);
  const { sort, handleSort, sortRows } = useTableSort<PersonalAccessToken>({
    defaultField: 'name',
    comparators: SORT_COMPARATORS,
  });

  /* ── search filtering (Warning #10) ─────────────────────────────────── */

  const filteredTokens = useMemo(() => {
    const query = search.toLowerCase().trim();
    if (!query) return tokens;
    return tokens.filter((t) =>
      t.name.toLowerCase().includes(query),
    );
  }, [tokens, search]);

  const sortedTokens = useMemo(() => sortRows(filteredTokens), [sortRows, filteredTokens]);

  /* ── columns ──────────────────────────────────────────────────────── */

  const columns = useMemo(
    (): GridTableColumn<PersonalAccessToken>[] =>
      columnsFor(tokens).map((column) => ({
        field: column.id,
        label: column.label,
        sortable: column.sortable,
        width: column.width,
        ...(column.id === 'actions' ? { align: 'right' as const } : {}),
        render: (row: PersonalAccessToken) => {
          switch (column.id) {
            case 'name':
              return row.name;
            case 'token':
              return maskedTokenValue(row);
            case 'project':
              return projectCellLabel(row, projectNames);
            case 'expires':
              return <ExpiryCell expires={row.expires} />;
            case 'administration':
              return <TokenAdminAccessCell token={row} />;
            case 'actions':
              return (
                <ActionsCell
                  token={row}
                  onDelete={(uuid) => deleteMutation.mutateAsync(uuid)}
                  onPreview={onPreviewToken ?? (() => {})}
                  showPreview={showPreview}
                />
              );
          }
        },
      })),
    [tokens, projectNames, deleteMutation, onPreviewToken, showPreview],
  );
  const { visibleColumns, gridTemplateColumns } = useResponsiveColumns({ columns });

  /* ── skeletons while loading ──────────────────────────────────────── */

  if (isFetching) {
    return (
      <Box sx={styles.loadingContainer}>
        {Array.from({ length: 5 }).map((_, i) => (
          <Box key={i} sx={styles.skeleton} />
        ))}
      </Box>
    );
  }

  /* ── table ────────────────────────────────────────────────────────── */

  return (
    <GridTable
      isEmpty={sortedTokens.length === 0}
      emptyMessage={t('entities.token.table.empty', 'No tokens')}
    >
      <GridTableHeader
        columns={visibleColumns}
        gridTemplateColumns={gridTemplateColumns}
        sort={sort}
        onSort={handleSort}
      />
      <GridTableBody>
        {sortedTokens.map((row) => (
          <GridTableRow
            key={row.uuid}
            row={row}
            columns={visibleColumns}
            gridTemplateColumns={gridTemplateColumns}
          />
        ))}
      </GridTableBody>
    </GridTable>
  );
});
