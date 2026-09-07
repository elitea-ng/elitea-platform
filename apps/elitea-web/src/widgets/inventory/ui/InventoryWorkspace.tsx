/**
 * One Inventory toolkit's workspace: sources, the graph, statistics, and the
 * panel that asks questions about it.
 *
 * THE LEGACY WAS ONE SCREEN WITH A CANVAS; this is three tabs and a drawer.
 * The canvas is what a 320-pixel drawer and a 300-pixel rail were arranged
 * around, and reproducing it would mean shipping a graph-layout engine to
 * render six nodes. What the canvas was FOR — see what an entity is, see what
 * it touches, walk from one to the next — is the graph tab, and every one of
 * those moves is a click here as it was there.
 *
 * THE SELECTION IS THE WIDGET'S, not a feature's. The list, the detail pane
 * and the ask panel's citations all move it, and `no-sideways-features`
 * forbids one feature reaching into another — so the state that all three
 * share lives at the one place that composes them.
 *
 * OPENING AN ENTITY SWITCHES TO THE GRAPH TAB. A citation clicked in the ask
 * drawer would otherwise change a selection on a tab the user cannot see, and
 * the click would read as having done nothing.
 */
import { useCallback, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Drawer from '@mui/material/Drawer';
import type { SxProps, Theme } from '@mui/material/styles';

import { useQueryClient } from '@tanstack/react-query';

import {
  inventoryConfig,
  inventoryToolkitToolsKey,
  useSourceToolkitNames,
  type InventorySettings,
  type InventoryTarget,
} from '@/entities/inventory';
import {
  EMPTY_GRAPH_FILTER,
  EntityDetail,
  EntityList,
  GraphFilters,
  useEntityDetail,
  useGraphBrowser,
  useGraphFacets,
  type GraphFilter,
} from '@/features/inventory-browser';
import { InventoryAskPanel, useInventoryAsk } from '@/features/inventory-chat';
import {
  AddSourceDialog,
  InventorySourcesPanel,
  SOURCE_TOOLKIT_TYPES,
  useIngestionRun,
  useInventorySources,
  useSaveSources,
  type SourceCandidate,
} from '@/features/inventory-sources';
import {
  InventoryStatsPanel,
  useInventoryStats,
  useMaintenance,
} from '@/features/inventory-stats';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseTab } from '@/shared/ui/BaseTab';
import { BaseTabs } from '@/shared/ui/BaseTabs';

import { IngestionBanner } from './IngestionBanner';

/**
 * The app's page recipe (pages/toolkits/Toolkits.tsx): the screen owns its
 * height, its tab bar and its scroll container, because the shell's `<main>`
 * supplies none of them and native scrollbars are hidden app-wide — a panel
 * without `overflowY: 'auto'` here could not be scrolled at all.
 */
const pageSx: SxProps<Theme> = { height: '100%', display: 'flex', flexDirection: 'column' };
const tabBarSx: SxProps<Theme> = {
  flexShrink: 0,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  gap: 1,
  borderBottom: 1,
  borderColor: 'divider',
  padding: '0 1.5rem',
};
const tabPanelSx: SxProps<Theme> = { flex: 1, minHeight: 0, overflowY: 'auto', padding: '1.5rem' };
const graphSx: SxProps<Theme> = {
  display: 'grid',
  gridTemplateColumns: { xs: '1fr', md: 'minmax(0, 2fr) minmax(0, 1fr)' },
  gap: 3,
  alignItems: 'start',
};
const drawerPaperSx: SxProps<Theme> = { width: { xs: '100%', sm: '28rem' }, padding: '1.5rem' };

type WorkspaceTab = 'sources' | 'graph' | 'stats';

export interface InventoryWorkspaceProps {
  readonly projectId: string;
  readonly toolkitId: string;
  readonly settings: InventorySettings;
  /**
   * The toolkit row as last read. Saving a source list is a PUT that REPLACES
   * the resource, so the request has to carry every field the row already had.
   */
  readonly toolkit: Readonly<Record<string, unknown>>;
}

export function InventoryWorkspace({
  projectId,
  toolkitId,
  settings,
  toolkit,
}: InventoryWorkspaceProps): React.JSX.Element {
  const target = useMemo<InventoryTarget>(
    () => ({ projectId, toolkitId, settings }),
    [projectId, toolkitId, settings],
  );
  const queryClient = useQueryClient();
  const [tab, setTab] = useState<WorkspaceTab>('sources');
  const [filter, setFilter] = useState<GraphFilter>(EMPTY_GRAPH_FILTER);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [askOpen, setAskOpen] = useState(false);
  const [addSourceOpen, setAddSourceOpen] = useState(false);

  const openEntity = useCallback((entityId: string) => {
    setSelectedId(entityId);
    setTab('graph');
  }, []);

  const invalidateGraph = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: inventoryToolkitToolsKey(target) });
  }, [queryClient, target]);

  const sources = useInventorySources(target, { enabled: tab === 'sources' });
  const ingestion = useIngestionRun(target, invalidateGraph);
  const browser = useGraphBrowser(target, filter, { enabled: tab === 'graph' });
  const facets = useGraphFacets(target, { enabled: tab === 'graph' || tab === 'stats' });
  const detail = useEntityDetail(target, tab === 'graph' ? selectedId : null);
  const stats = useInventoryStats(target, { enabled: tab === 'stats' });
  const maintenance = useMaintenance(target);
  const ask = useInventoryAsk(target);
  const saveSources = useSaveSources();
  const projectToolkits = useSourceToolkitNames(projectId);

  const configuredSourceIds = useMemo(
    () => inventoryConfig.sourceIds(settings),
    [settings],
  );

  /**
   * Every repository toolkit in the project that is not already a source.
   *
   * The Inventory toolkit ITSELF is excluded by the type filter — it is of
   * type `inventory`, not one of the four the descriptor admits — so no
   * separate guard is needed against an inventory ingesting itself.
   */
  const candidates = useMemo<readonly SourceCandidate[]>(() => {
    const rows: SourceCandidate[] = [];
    for (const [id, row] of projectToolkits.data ?? []) {
      if (!SOURCE_TOOLKIT_TYPES.includes(row.type.toLowerCase())) continue;
      if (configuredSourceIds.includes(id)) continue;
      rows.push({ id, name: row.name, type: row.type });
    }
    return rows;
  }, [projectToolkits.data, configuredSourceIds]);

  /**
   * Whether every eligible toolkit is already a source.
   *
   * A DIFFERENT MESSAGE from "there is nothing to add": one says go and create
   * a repository toolkit, the other says you already added them all, and a
   * dialog that showed the first to a user who has added everything sends them
   * to create a toolkit they do not need.
   */
  const allEligibleAdded = useMemo(() => {
    let eligible = 0;
    for (const [, row] of projectToolkits.data ?? []) {
      if (SOURCE_TOOLKIT_TYPES.includes(row.type.toLowerCase())) eligible += 1;
    }
    return eligible > 0 && candidates.length === 0;
  }, [projectToolkits.data, candidates.length]);

  const saveSourceIds = useCallback(
    (sourceIds: readonly string[]) => {
      saveSources.mutate({ projectId, toolkitId, toolkit, settings, sourceIds });
    },
    [saveSources, projectId, toolkitId, toolkit, settings],
  );

  return (
    <Box sx={pageSx} data-testid="inventory-workspace">
      <Box sx={tabBarSx}>
        <BaseTabs
          value={tab}
          onChange={(_event, value: WorkspaceTab) => {
            setTab(value);
          }}
        >
          <BaseTab value="sources" label={t('inventory.tab.sources', 'Sources')} data-testid="inventory-tab-sources" />
          <BaseTab value="graph" label={t('inventory.tab.graph', 'Graph')} data-testid="inventory-tab-graph" />
          <BaseTab value="stats" label={t('inventory.tab.stats', 'Statistics')} data-testid="inventory-tab-stats" />
        </BaseTabs>
        <BaseBtn
          variant="secondary"
          size="small"
          data-testid="inventory-open-ask"
          onClick={() => {
            setAskOpen(true);
          }}
        >
          {t('inventory.ask.open', 'Ask this inventory')}
        </BaseBtn>
      </Box>

      <Box sx={tabPanelSx}>
        {tab === 'sources' ? (
          <Box>
            <IngestionBanner run={ingestion} />
            <InventorySourcesPanel
              sources={sources.sources}
              ingestion={sources.ingestion}
              isPending={sources.isPending}
              statusError={sources.statusError}
              runningSourceId={ingestion.runningSourceId}
              canIngest={inventoryConfig.llmModel(settings) !== ''}
              onIngest={(sourceToolkitId) => {
                ingestion.start(sourceToolkitId, false);
              }}
              onStop={ingestion.stop}
              onAddSource={() => {
                setAddSourceOpen(true);
              }}
              onRemoveSource={(sourceToolkitId) => {
                saveSourceIds(configuredSourceIds.filter((id) => id !== sourceToolkitId));
              }}
            />
            <AddSourceDialog
              open={addSourceOpen}
              candidates={candidates}
              allAdded={allEligibleAdded}
              onAdd={(sourceToolkitId) => {
                setAddSourceOpen(false);
                saveSourceIds([...configuredSourceIds, sourceToolkitId]);
              }}
              onClose={() => {
                setAddSourceOpen(false);
              }}
            />
          </Box>
        ) : null}

        {tab === 'graph' ? (
          <Box sx={graphSx}>
            <Box>
              <GraphFilters
                filter={filter}
                types={facets.types}
                layers={facets.layers}
                sources={facets.sources}
                onChange={setFilter}
              />
              <EntityList
                entities={browser.entities}
                selectedId={selectedId}
                isPending={browser.isPending}
                error={browser.error}
                onSelect={(entity) => {
                  setSelectedId(entity.id);
                }}
              />
            </Box>
            <EntityDetail
              entity={detail.entity}
              neighbours={detail.neighbours}
              isPending={detail.isPending}
              error={detail.error}
              onOpenNeighbour={openEntity}
            />
          </Box>
        ) : null}

        {tab === 'stats' ? (
          <InventoryStatsPanel
            stats={stats.stats}
            cachedGraphs={stats.cachedGraphs}
            cacheSizeBytes={stats.cacheSizeBytes}
            isPending={stats.isPending}
            error={stats.error}
            maintenanceRunning={maintenance.running}
            maintenanceMessage={maintenance.message}
            maintenanceError={maintenance.error}
            onRunMaintenance={maintenance.run}
          />
        ) : null}
      </Box>

      <Drawer
        anchor="right"
        open={askOpen}
        slotProps={{ paper: { sx: drawerPaperSx } }}
        onClose={() => {
          setAskOpen(false);
        }}
      >
        {/* The test id is on the CONTENT, not on the paper: MUI's Drawer
            paper slot props are typed to PaperProps, which does not admit a
            data attribute, and casting the slot props to get one there would
            put a cast in the composition root to name an element that is
            already here. */}
        <InventoryAskPanel
          turns={ask.turns}
          pendingQuestion={ask.pendingQuestion}
          steps={ask.steps}
          error={ask.error}
          onAsk={ask.ask}
          onStop={ask.stop}
          onClear={ask.clear}
          onOpenEntity={(entityId) => {
            setAskOpen(false);
            openEntity(entityId);
          }}
        />
      </Drawer>
    </Box>
  );
}
