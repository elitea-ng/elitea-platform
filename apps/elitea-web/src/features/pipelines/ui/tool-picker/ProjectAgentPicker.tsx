import { memo, useCallback, useContext, useMemo, type ReactNode } from 'react';

import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import { PIPELINE_HOST_TIER } from '../../lib/tool-picker/agentNesting.helpers';
import { buildApplicationPickerItems } from '../../lib/tool-picker/pickerItems.helpers';
import {
  collectToolReferences,
  getReferencedNames,
  isApplicationTool,
} from '../../lib/tool-picker/pipelineToolUsage.helpers';
import { TOOL_REF_KINDS } from '../../lib/tool-picker/toolPicker.constants';
import type {
  ApplicationListRow,
  PickerCreateAction,
  PickerGroup,
  PickerItem,
  PickerToolEntry,
  PipelineYamlDocument,
} from '../../lib/tool-picker/toolPicker.types';
import { useCreateInNewTab } from '../../lib/tool-picker/useCreateInNewTab';
import { useIsVersionLocked } from '../../lib/tool-picker/useIsVersionLocked';
import { usePickerMenu } from '../../lib/tool-picker/usePickerMenu';
import { useProjectApplicationsList } from '../../lib/tool-picker/useProjectApplicationsList';
import { PickerEntityIcon } from './PickerEntityIcon';
import { PickerItemRow } from './PickerItemRow';
import { PickerMenu } from './PickerMenu';
import { PickerTrigger } from './PickerTrigger';

const CREATE_APPLICATION_ROUTE = '/agents/create';
const CREATE_PIPELINE_ROUTE = '/pipelines/create';

function isPipelineSource(source: PickerToolEntry | ApplicationListRow): boolean {
  return source.agent_type === 'pipeline' || source.version_details?.agent_type === 'pipeline';
}

function resolveAgentGroupRows(
  attachedOnly: boolean,
  attachedTools: readonly PickerToolEntry[],
  fetchedRows: readonly ApplicationListRow[],
  pipelineGroup: boolean,
): readonly ApplicationListRow[] {
  if (!attachedOnly) return fetchedRows;
  return attachedTools.filter((tool) => isPipelineSource(tool) === pipelineGroup) as unknown as readonly ApplicationListRow[];
}

export interface ProjectAgentPickerContext {
  readonly attachedTools?: readonly PickerToolEntry[] | undefined;
  readonly excludeId?: number | string | undefined;
  readonly versionStatus?: string | undefined;
  readonly projectId?: string | undefined;
  readonly yamlJsonObject?: PipelineYamlDocument;
}

export interface ProjectAgentPickerProps {
  readonly label?: string | undefined;
  readonly tooltip?: ReactNode | undefined;
  readonly value?: string | undefined;
  readonly onSelect?: ((item: PickerItem<PickerToolEntry | ApplicationListRow>) => void) | undefined;
  readonly onClear?: (() => void) | undefined;
  readonly disabled?: boolean | undefined;
  readonly attachedOnly?: boolean | undefined;
  readonly pickerContext?: ProjectAgentPickerContext | undefined;
  readonly 'data-testid'?: string | undefined;
}

function resolveAgentPickerContext(
  pickerContext: ProjectAgentPickerContext | undefined,
  flowYaml: PipelineYamlDocument,
) {
  return {
    yamlJsonObject: pickerContext?.yamlJsonObject ?? flowYaml,
    versionStatus: pickerContext?.versionStatus,
    projectId: pickerContext?.projectId,
    excludeId: pickerContext?.excludeId,
    rawAttached: pickerContext?.attachedTools ?? [],
  };
}

function suffixTestId(testId: string | undefined, suffix: string): string | undefined {
  return testId ? `${testId}-${suffix}` : undefined;
}

/**
 * Picks any agent or pipeline of the project for an Agent node.
 * Disables Swarm, over-nested, interrupting, and duplicate-named candidates with tooltip explanations (EL-7085).
 */
export const ProjectAgentPicker = memo(function ProjectAgentPicker({
  label,
  tooltip,
  value,
  onSelect,
  onClear,
  disabled = false,
  attachedOnly = false,
  pickerContext,
  'data-testid': testId,
}: ProjectAgentPickerProps): ReactNode {
  const flowContext = useContext(FlowEditorContext);
  const { yamlJsonObject, versionStatus, projectId, excludeId, rawAttached } = resolveAgentPickerContext(
    pickerContext,
    flowContext?.yamlJsonObject,
  );
  const isVersionLocked = useIsVersionLocked(versionStatus);

  const resolvedLabel = label ?? t('pipelines.toolPicker.agentsAndPipelinesLabel', 'Agents & Pipelines');

  const { anchorEl, isOpen, hasOpened, search, setSearch, query, openMenu, closeMenu } = usePickerMenu();
  const skipFetch = attachedOnly || !hasOpened;
  const agents = useProjectApplicationsList({
    agentsType: 'classic',
    query,
    skip: skipFetch,
    projectId,
  });
  const pipelines = useProjectApplicationsList({
    agentsType: 'pipeline',
    query,
    skip: skipFetch,
    projectId,
  });

  const reload = useCallback(() => {
    agents.reload();
    pipelines.reload();
  }, [agents, pipelines]);
  const openCreatePage = useCreateInNewTab(reload);

  const attachedTools = useMemo(() => rawAttached.filter(isApplicationTool), [rawAttached]);
  const usedNames = useMemo(
    () => getReferencedNames(collectToolReferences(yamlJsonObject), TOOL_REF_KINDS.application),
    [yamlJsonObject],
  );

  const buildItems = useCallback(
    (rows: readonly ApplicationListRow[], pipelineGroup: boolean) =>
      buildApplicationPickerItems({
        rows,
        attachedTools: attachedTools.filter((tool) => isPipelineSource(tool) === pipelineGroup),
        allAttachedTools: attachedTools,
        usedNames,
        query: search,
        excludeId,
        hostTier: PIPELINE_HOST_TIER,
      }).map((item) => ({ ...item, isPipeline: pipelineGroup })),
    [attachedTools, excludeId, search, usedNames],
  );

  const groups = useMemo<PickerGroup<PickerItem<PickerToolEntry | ApplicationListRow>>[]>(
    () => [
      {
        key: 'agents',
        title: t('pipelines.toolPicker.groupAgents', 'Agents'),
        items: buildItems(resolveAgentGroupRows(attachedOnly, attachedTools, agents.rows, false), false),
      },
      {
        key: 'pipelines',
        title: t('pipelines.toolPicker.groupPipelines', 'Pipelines'),
        items: buildItems(resolveAgentGroupRows(attachedOnly, attachedTools, pipelines.rows, true), true),
      },
    ],
    [agents.rows, attachedOnly, attachedTools, buildItems, pipelines.rows],
  );

  const handleSelect = useCallback(
    (item: PickerItem<PickerToolEntry | ApplicationListRow>) => {
      closeMenu();
      if (item.name !== value) onSelect?.(item);
    },
    [closeMenu, onSelect, value],
  );

  const onLoadMore = useCallback(() => {
    if (agents.hasMore) agents.onLoadMore();
    else pipelines.onLoadMore();
  }, [agents, pipelines]);

  const createActions = useMemo<PickerCreateAction[]>(
    () => [
      {
        key: 'agent',
        label: t('pipelines.toolPicker.createNewAgent', 'Create new agent'),
        onClick: () => {
          closeMenu();
          openCreatePage(CREATE_APPLICATION_ROUTE);
        },
        testId: suffixTestId(testId, 'create-agent'),
      },
      {
        key: 'pipeline',
        label: t('pipelines.toolPicker.createNewPipeline', 'Create new pipeline'),
        onClick: () => {
          closeMenu();
          openCreatePage(CREATE_PIPELINE_ROUTE);
        },
        testId: suffixTestId(testId, 'create-pipeline'),
      },
    ],
    [closeMenu, openCreatePage, testId],
  );

  const renderItem = useCallback(
    (item: PickerItem<PickerToolEntry | ApplicationListRow>) => (
      <PickerItemRow
        key={item.key}
        item={item}
        selected={item.name === value}
        onSelect={handleSelect}
        icon={
          <PickerEntityIcon
            source={item.source}
            isApplication
            isPipeline={item.isPipeline}
          />
        }
        data-testid={suffixTestId(testId, 'option')}
      />
    ),
    [handleSelect, testId, value],
  );

  const loadState = useMemo(
    () => ({
      isLoading: agents.isFetching || pipelines.isFetching,
      isError: agents.isError || pipelines.isError,
      onRetry: reload,
      onLoadMore,
    }),
    [agents.isError, agents.isFetching, onLoadMore, pipelines.isError, pipelines.isFetching, reload],
  );

  const messages = useMemo(
    () => ({
      searchPlaceholder: t(
        'pipelines.toolPicker.searchAgentsAndPipelines',
        'Search agents and pipelines...',
      ),
      emptyMessage: t(
        'pipelines.toolPicker.emptyAgentsAndPipelines',
        'No agents or pipelines in this project yet',
      ),
      noResultsMessage: t(
        'pipelines.toolPicker.noResultsAgentsAndPipelines',
        'No agents or pipelines found',
      ),
    }),
    [],
  );

  return (
    <>
      <PickerTrigger
        tooltip={tooltip}
        label={resolvedLabel}
        hasValue={Boolean(value)}
        isOpen={isOpen}
        disabled={disabled || isVersionLocked}
        onOpen={openMenu}
        onClear={onClear}
        data-testid={testId}
      >
        <Typography variant="labelMedium" color="inherit" noWrap>
          {value}
        </Typography>
      </PickerTrigger>
      <PickerMenu
        anchorEl={anchorEl}
        open={isOpen}
        onClose={closeMenu}
        searchValue={search}
        onSearchChange={setSearch}
        createActions={createActions}
        groups={groups}
        renderItem={renderItem}
        loadState={loadState}
        messages={messages}
        data-testid={suffixTestId(testId, 'menu')}
      />
    </>
  );
});
