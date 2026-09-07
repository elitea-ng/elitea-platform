/**
 * `Admin › Toolkits` — the native control over which toolkit TYPES this
 * platform offers, and to which projects (shared migration 0114).
 *
 * ## What this page is, and what it is not
 *
 * The served type catalogue decides which tiles a user sees in the "+ Toolkit"
 * chooser. Before this page an operator could influence that in two ways: the
 * guardrails DENY-list on the Configuration page, which is deployment-wide,
 * carries no provenance and cannot say WHY a type is off; or a code change.
 *
 * This page decides, per type: enabled, disabled, or restricted to named
 * projects — always with a written reason and a recorded decider.
 *
 * IT DOES NOT ADD A TYPE. Two surfaces already do that, and a third
 * registration path would be a third thing to keep in step: a pre-built MCP
 * server becomes a type through `Admin › Configuration`, and an external
 * provider's toolkits arrive through `Admin › Service Descriptors`. The notice
 * at the top of this page points at both, rather than growing an add form here.
 *
 * ## The worker column is advisory
 *
 * `Unverified` reports that neither the pinned Python worker image nor the Rust
 * worker is KNOWN to carry the type. It is not a claim that the type fails, and
 * nothing on this page filters on it. The server sends a sentence with every
 * verdict, and the table shows it, so the chip cannot be read alone.
 *
 * ## An unreadable registry says so
 *
 * `registryAvailable: false` renders a notice, not an empty grid. Zero toolkit
 * types is not a state this platform can be in, so an empty list would tell the
 * operator their platform has no toolkits — which is alarming and wrong.
 */
import { useCallback, useMemo, useState } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import AlertTitle from '@mui/material/AlertTitle';
import LinearProgress from '@mui/material/LinearProgress';
import MenuItem from '@mui/material/MenuItem';
import Stack from '@mui/material/Stack';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { ToolkitTypeBulkDialog } from './ToolkitTypeBulkDialog';
import { ToolkitTypeDecisionDialog } from './ToolkitTypeDecisionDialog';
import { ToolkitTypeProjectsDialog } from './ToolkitTypeProjectsDialog';
import { AdminToolkitTypesTable } from './ToolkitTypesTable';
import {
  toolkitTypeFailureReason,
  toolkitTypeFailureStatus,
  useAdminToolkitTypes,
  type AdminToolkitType,
} from './api/adminToolkitTypesApi';

/**
 * Which dialog is open, and for which row.
 *
 * IT HOLDS THE TYPE KEY, NOT THE ROW. The projects dialog renders the grants it
 * is given, and a write invalidates the listing; a captured row object would go
 * on rendering the grants as they were when the dialog opened, so an operator
 * would add an exception and watch nothing appear. The live row is looked up
 * from the query on every render.
 */
interface OpenDialog {
  readonly kind: 'decide' | 'projects';
  readonly toolkitType: string;
}

/**
 * The category filter's "everything" option.
 *
 * A literal that cannot collide with a category identifier, because the server
 * sends those and this page must not silently swallow one that happens to be
 * spelled `all`.
 */
const ALL_CATEGORIES = '__all__';

/** Stable empty defaults, so an absent query result is one identity, not many. */
const EMPTY_TYPES: readonly AdminToolkitType[] = [];
const EMPTY_CATEGORIES: readonly string[] = [];

function matchesSearch(row: AdminToolkitType, search: string): boolean {
  if (search === '') return true;
  const needle = search.toLowerCase();
  // The TYPE KEY is searched as well as the label. An operator reading a
  // guardrails list or a server log has the key, not the label, and a search
  // that only matched labels would fail exactly when they need it.
  return row.label.toLowerCase().includes(needle) || row.type.toLowerCase().includes(needle);
}

/**
 * The three things that can be wrong with this page, each with its own testid.
 *
 * A separate component because the page's own branch count is budgeted, and
 * because the three states are mutually exclusive in a way that is easier to
 * read here than interleaved with the grid.
 */
function ToolkitTypeNotices({
  isUnavailable,
  isError,
  registryUnavailable,
  reason,
}: {
  readonly isUnavailable: boolean;
  readonly isError: boolean;
  readonly registryUnavailable: boolean;
  readonly reason: string | undefined;
}) {
  return (
    <>
      {isUnavailable ? (
        <Alert severity="info" data-testid="admin-toolkit-types-unavailable">
          <AlertTitle>
            {t('pages.admin.toolkitTypes.unavailableTitle', 'Toolkit type policy is unavailable')}
          </AlertTitle>
          {reason}
        </Alert>
      ) : null}
      {isError && !isUnavailable ? (
        <Alert severity="warning" data-testid="admin-toolkit-types-error">
          {reason ?? t('pages.admin.toolkitTypes.error.load', 'Failed to load the toolkit types.')}
        </Alert>
      ) : null}
      {registryUnavailable ? (
        <Alert severity="warning" data-testid="admin-toolkit-types-registry-unavailable">
          {t(
            'pages.admin.toolkitTypes.registryUnavailable',
            'This deployment cannot enumerate its toolkit registry, so only the types it has already decided about are listed.',
          )}
        </Alert>
      ) : null}
    </>
  );
}

export function AdminToolkitTypes() {
  const query = useAdminToolkitTypes();

  const [search, setSearch] = useState('');
  const [category, setCategory] = useState(ALL_CATEGORIES);
  const [dialog, setDialog] = useState<OpenDialog | null>(null);
  const [bulkOpen, setBulkOpen] = useState(false);

  const onDecide = useCallback((row: AdminToolkitType) => {
    setDialog({ kind: 'decide', toolkitType: row.type });
  }, []);
  const onProjects = useCallback((row: AdminToolkitType) => {
    setDialog({ kind: 'projects', toolkitType: row.type });
  }, []);
  const closeDialog = useCallback(() => setDialog(null), []);
  const openBulk = useCallback(() => setBulkOpen(true), []);
  const closeBulk = useCallback(() => setBulkOpen(false), []);

  // Memoised against the query result, not derived inline: a fresh array on
  // every render makes every dependent useMemo run on every render, which oxlint
  // reports and which would rebuild the row list on each keystroke.
  const types = useMemo(() => query.data?.types ?? EMPTY_TYPES, [query.data]);
  const categories = query.data?.categories ?? EMPTY_CATEGORIES;

  const visible = useMemo(
    () =>
      types.filter(
        (row) =>
          matchesSearch(row, search.trim()) &&
          (category === ALL_CATEGORIES || row.category === category),
      ),
    [category, search, types],
  );

  const visibleTypeKeys = useMemo(() => visible.map((row) => row.type), [visible]);

  const openRow = useMemo(
    () => (dialog === null ? null : (types.find((row) => row.type === dialog.toolkitType) ?? null)),
    [dialog, types],
  );

  const reason = toolkitTypeFailureReason(query.error);
  const status = toolkitTypeFailureStatus(query.error);
  // 501 and 503 both mean "this deployment does not have the surface", and the
  // page renders the server's own sentence for them. Anything else is a load
  // failure and reads as one.
  const isUnavailable = status === 501 || status === 503;

  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }}>
      <Typography variant="h5" sx={{ fontWeight: 600 }}>
        {t('pages.admin.toolkitTypes.title', 'Toolkits')}
      </Typography>

      <Typography variant="body2" color="text.secondary">
        {t(
          'pages.admin.toolkitTypes.intro',
          'Decide which toolkit types this platform offers, and to which projects. To add a new type, catalogue an MCP server on the Configuration page or admit a provider on the Service Descriptors page.',
        )}
      </Typography>

      {query.isPending ? <LinearProgress /> : null}

      <ToolkitTypeNotices
        isUnavailable={isUnavailable}
        isError={query.isError}
        registryUnavailable={query.isSuccess && !query.data.registryAvailable}
        reason={reason}
      />

      {query.isSuccess ? (
        <>
          <Stack direction="row" spacing={1}>
            <TextField
              size="small"
              label={t('pages.admin.toolkitTypes.search', 'Search')}
              value={search}
              onChange={(event) => setSearch(event.target.value)}
              sx={{ width: '18rem' }}
            />
            <TextField
              select
              size="small"
              label={t('pages.admin.toolkitTypes.category', 'Category')}
              value={category}
              onChange={(event) => setCategory(event.target.value)}
              sx={{ width: '14rem' }}
            >
              <MenuItem value={ALL_CATEGORIES}>
                {t('pages.admin.toolkitTypes.allCategories', 'All categories')}
              </MenuItem>
              {categories.map((value) => (
                <MenuItem key={value} value={value}>
                  {value}
                </MenuItem>
              ))}
            </TextField>
            {/* The subject is the FILTERED set, so what the operator sees is
                what they change. */}
            <Button onClick={openBulk} disabled={visible.length === 0}>
              {t('pages.admin.toolkitTypes.action.bulk', 'Apply to listed')}
            </Button>
          </Stack>
          <AdminToolkitTypesTable types={visible} onDecide={onDecide} onProjects={onProjects} />
        </>
      ) : null}

      <ToolkitTypeDecisionDialog
        toolkitType={dialog?.kind === 'decide' ? openRow : null}
        onClose={closeDialog}
      />
      <ToolkitTypeBulkDialog
        open={bulkOpen}
        types={visibleTypeKeys}
        onClose={closeBulk}
      />
      <ToolkitTypeProjectsDialog
        toolkitType={dialog?.kind === 'projects' ? openRow : null}
        onClose={closeDialog}
      />
    </DrawerPage>
  );
}
