import { memo, useCallback, useContext, useMemo, type ReactNode } from 'react';

import Chip from '@mui/material/Chip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { useIsMcpVisible } from '../../api/useIsMcpVisible';
import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import { useGetToolkitNameFromSchema } from '../../lib/flow-editor/hooks/useGetToolkitNameFromSchema';
import { useSelectedProjectId } from '../../lib/flow-editor/hooks/useSelectedProjectId';
import { useToolkitTypeSchemas } from '../../lib/flow-editor/hooks/useToolkitTypeSchemas';
import { buildToolkitPickerItems } from '../../lib/tool-picker/pickerItems.helpers';
import {
  collectToolReferences,
  getReferencedNames,
  getToolkitDisplayName,
  isApplicationTool,
  isMcpAttachment,
} from '../../lib/tool-picker/pipelineToolUsage.helpers';
import { TOOL_REF_KINDS } from '../../lib/tool-picker/toolPicker.constants';
import type {
  PickerCreateAction,
  PickerGroup,
  PickerItem,
  PickerToolEntry,
  PipelineYamlDocument,
  ToolkitListRow,
} from '../../lib/tool-picker/toolPicker.types';
import { useCreateInNewTab } from '../../lib/tool-picker/useCreateInNewTab';
import { useIsVersionLocked } from '../../lib/tool-picker/useIsVersionLocked';
import { usePickerMenu } from '../../lib/tool-picker/usePickerMenu';
import { useProjectToolkitsList } from '../../lib/tool-picker/useProjectToolkitsList';
import { buildAttachedToolkitPickerItems } from './attachedOnlyPickerItems.helpers';
import { PickerEntityIcon } from './PickerEntityIcon';
import { PickerItemRow } from './PickerItemRow';
import { PickerMcpStatus } from './PickerMcpStatus';
import { PickerMenu } from './PickerMenu';
import { PickerTrigger } from './PickerTrigger';

const CREATE_TOOLKIT_ROUTE = '/toolkits/create';
const CREATE_MCP_ROUTE = '/mcps/create';

const chipSx: SxProps<Theme> = (theme: Theme) => ({
  maxWidth: '100%',
  backgroundColor: theme.vars.palette.background.tagChip.default,
});

export interface ProjectToolkitsMultiPickerContext {
  readonly attachedTools?: readonly PickerToolEntry[] | undefined;
  readonly versionStatus?: string | undefined;
  readonly projectId?: string | undefined;
  readonly yamlJsonObject?: PipelineYamlDocument;
  readonly onMcpLoginClick?: ((mcp: PickerToolEntry | ToolkitListRow) => void) | undefined;
}

export interface ProjectToolkitsMultiPickerProps {
  readonly label?: string | undefined;
  readonly tooltip?: ReactNode | undefined;
  readonly value?: readonly string[] | undefined;
  readonly onAdd?: ((source: PickerToolEntry | ToolkitListRow, toolkitName: string, mcp: boolean) => void) | undefined;
  readonly onRemove?: ((toolkitName: string) => void) | undefined;
  readonly onClear?: (() => void) | undefined;
  readonly disabled?: boolean | undefined;
  readonly attachedOnly?: boolean | undefined;
  readonly pickerContext?: ProjectToolkitsMultiPickerContext | undefined;
  readonly 'data-testid'?: string | undefined;
}

function getMultiPickerLabel(isMcpVisible: boolean, label: string | undefined): string {
  if (label) return label;
  return isMcpVisible
    ? t('pipelines.toolPicker.toolkitsAndMcpsLabel', 'Toolkits & MCPs')
    : t('pipelines.toolPicker.toolkitsLabel', 'Toolkits');
}

function getMultiPickerMessages(isMcpVisible: boolean) {
  return {
    searchPlaceholder: isMcpVisible
      ? t('pipelines.toolPicker.searchToolkitsAndMcps', 'Search toolkits and MCPs...')
      : t('pipelines.toolPicker.searchToolkits', 'Search toolkits...'),
    emptyMessage: t('pipelines.toolPicker.emptyToolkits', 'No toolkits in this project yet'),
    noResultsMessage: t('pipelines.toolPicker.noResultsToolkits', 'No toolkits found'),
  };
}

function buildMultiCreateActions(
  isMcpVisible: boolean,
  closeMenu: () => void,
  openCreatePage: (route: string) => void,
  testId: string | undefined,
): PickerCreateAction[] {
  const actions: PickerCreateAction[] = [
    {
      key: 'toolkit',
      label: t('pipelines.toolPicker.createNewToolkit', 'Create new toolkit'),
      onClick: () => {
        closeMenu();
        openCreatePage(CREATE_TOOLKIT_ROUTE);
      },
      testId: testId ? `${testId}-create-toolkit` : undefined,
    },
  ];
  if (isMcpVisible) {
    actions.push({
      key: 'mcp',
      label: t('pipelines.toolPicker.createNewMcp', 'Create new MCP'),
      onClick: () => {
        closeMenu();
        openCreatePage(CREATE_MCP_ROUTE);
      },
      testId: testId ? `${testId}-create-mcp` : undefined,
    });
  }
  return actions;
}

function MultiPickerChips({
  value,
  isDisabled,
  onRemove,
}: {
  readonly value: readonly string[];
  readonly isDisabled: boolean;
  readonly onRemove: ((toolkitName: string) => void) | undefined;
}): ReactNode {
  return value.map((name) => (
    <Chip
      key={name}
      size="small"
      sx={chipSx}
      label={
        <Typography variant="labelSmall" color="text.secondary">
          {name}
        </Typography>
      }
      onDelete={isDisabled ? undefined : () => onRemove?.(name)}
      onMouseDown={(event) => event.stopPropagation()}
      onClick={(event) => event.stopPropagation()}
    />
  ));
}

function resolveMultiContext(
  pickerContext: ProjectToolkitsMultiPickerContext | undefined,
  flowYaml: PipelineYamlDocument,
  routeProjectId: string | undefined,
) {
  return {
    yamlJsonObject: pickerContext?.yamlJsonObject ?? flowYaml,
    projectId: pickerContext?.projectId ?? routeProjectId,
    versionStatus: pickerContext?.versionStatus,
    attachedTools: pickerContext?.attachedTools,
    onMcpLoginClick: pickerContext?.onMcpLoginClick,
  };
}

/**
 * Picks toolkits and MCPs of the project for an LLM node. The menu stays open so several can be toggled.
 */
export const ProjectToolkitsMultiPicker = memo(function ProjectToolkitsMultiPicker({
  label,
  tooltip,
  value = [],
  onAdd,
  onRemove,
  onClear,
  disabled = false,
  attachedOnly = false,
  pickerContext,
  'data-testid': testId,
}: ProjectToolkitsMultiPickerProps): ReactNode {
  const flowContext = useContext(FlowEditorContext);
  const routeProjectId = useSelectedProjectId();
  const { yamlJsonObject, projectId, versionStatus, attachedTools, onMcpLoginClick } =
    resolveMultiContext(pickerContext, flowContext?.yamlJsonObject, routeProjectId);
  const { toolkitTypeSchemas } = useToolkitTypeSchemas(projectId);
  const { getToolkitNameFromSchema } = useGetToolkitNameFromSchema(toolkitTypeSchemas);
  const isVersionLocked = useIsVersionLocked(versionStatus);
  const isMcpVisible = useIsMcpVisible();

  const { anchorEl, isOpen, hasOpened, search, setSearch, query, openMenu, closeMenu } = usePickerMenu();
  const toolkits = useProjectToolkitsList({ mcp: false, query, skip: attachedOnly || !hasOpened, projectId });
  const mcps = useProjectToolkitsList({ mcp: true, query, skip: attachedOnly || !hasOpened || !isMcpVisible, projectId });

  const reload = useCallback(() => {
    toolkits.reload();
    if (isMcpVisible) mcps.reload();
  }, [isMcpVisible, mcps, toolkits]);
  const openCreatePage = useCreateInNewTab(reload);

  const usedNames = useMemo(
    () => getReferencedNames(collectToolReferences(yamlJsonObject), TOOL_REF_KINDS.toolkit),
    [yamlJsonObject],
  );

  const buildItems = useCallback(
    (rows: readonly ToolkitListRow[], mcp: boolean) => {
      const filteredAttached = (attachedTools ?? []).filter(
        (tool) => !isApplicationTool(tool) && isMcpAttachment(tool) === mcp,
      );
      if (attachedOnly) {
        return buildAttachedToolkitPickerItems({
          attachedTools: filteredAttached,
          usedNames,
          query: search,
          getToolkitName: getToolkitNameFromSchema,
          getLabel: getToolkitDisplayName,
          mcp,
        });
      }
      return buildToolkitPickerItems({
        rows,
        attachedTools: filteredAttached,
        usedNames,
        query: search,
        getToolkitName: getToolkitNameFromSchema,
        getLabel: getToolkitDisplayName,
      }).map((item) => ({ ...item, mcp }));
    },
    [attachedOnly, attachedTools, getToolkitNameFromSchema, search, usedNames],
  );

  const groups = useMemo<PickerGroup<PickerItem<PickerToolEntry | ToolkitListRow>>[]>(
    () => [
      {
        key: 'toolkits',
        title: t('pipelines.toolPicker.groupToolkits', 'Toolkits'),
        items: buildItems(toolkits.rows, false),
      },
      ...(isMcpVisible
        ? [
            {
              key: 'mcps',
              title: t('pipelines.toolPicker.groupMcps', 'MCPs'),
              items: buildItems(mcps.rows, true),
            },
          ]
        : []),
    ],
    [buildItems, isMcpVisible, mcps.rows, toolkits.rows],
  );

  const handleToggle = useCallback(
    (item: PickerItem<PickerToolEntry | ToolkitListRow>) => {
      if (value.includes(item.name)) {
        onRemove?.(item.name);
      } else {
        onAdd?.(item.source, item.name, Boolean(item.mcp));
      }
    },
    [onAdd, onRemove, value],
  );

  const onLoadMore = useCallback(() => {
    if (toolkits.hasMore) toolkits.onLoadMore();
    else if (isMcpVisible) mcps.onLoadMore();
  }, [isMcpVisible, mcps, toolkits]);

  const createActions = useMemo(
    () => buildMultiCreateActions(isMcpVisible, closeMenu, openCreatePage, testId),
    [closeMenu, isMcpVisible, openCreatePage, testId],
  );

  const renderItem = useCallback(
    (item: PickerItem<PickerToolEntry | ToolkitListRow>) => (
      <PickerItemRow
        key={item.key}
        item={item}
        selected={value.includes(item.name)}
        onSelect={handleToggle}
        icon={<PickerEntityIcon source={item.source} mcp={item.mcp} />}
        status={item.mcp ? <PickerMcpStatus mcp={item.source} onLoginClick={onMcpLoginClick} /> : null}
        data-testid={testId ? `${testId}-option` : undefined}
      />
    ),
    [handleToggle, onMcpLoginClick, testId, value],
  );

  const loadState = useMemo(
    () => ({
      isLoading: toolkits.isFetching || mcps.isFetching,
      isError: toolkits.isError || mcps.isError,
      onRetry: reload,
      onLoadMore,
    }),
    [mcps.isError, mcps.isFetching, onLoadMore, reload, toolkits.isError, toolkits.isFetching],
  );

  const messages = useMemo(() => getMultiPickerMessages(isMcpVisible), [isMcpVisible]);
  const resolvedLabel = getMultiPickerLabel(isMcpVisible, label);

  const isDisabled = disabled || isVersionLocked;

  const menuTestId = testId ? `${testId}-menu` : undefined;

  return (
    <>
      <PickerTrigger
        tooltip={tooltip}
        label={resolvedLabel}
        hasValue={value.length > 0}
        isOpen={isOpen}
        disabled={isDisabled}
        onOpen={openMenu}
        onClear={onClear}
        data-testid={testId}
      >
        <MultiPickerChips value={value} isDisabled={isDisabled} onRemove={onRemove} />
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
        data-testid={menuTestId}
      />
    </>
  );
});
