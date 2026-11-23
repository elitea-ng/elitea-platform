import { memo, useCallback, useContext, useMemo, type ReactNode } from 'react';

import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import { useGetToolkitNameFromSchema } from '../../lib/flow-editor/hooks/useGetToolkitNameFromSchema';
import { useSelectedProjectId } from '../../lib/flow-editor/hooks/useSelectedProjectId';
import { useToolkitTypeSchemas } from '../../lib/flow-editor/hooks/useToolkitTypeSchemas';
import { buildToolkitPickerItems } from '../../lib/tool-picker/pickerItems.helpers';
import {
  collectToolReferences,
  getAttachmentRefName,
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
import { PickerEntityIcon } from './PickerEntityIcon';
import { PickerItemRow } from './PickerItemRow';
import { PickerMcpStatus } from './PickerMcpStatus';
import { PickerMenu } from './PickerMenu';
import { PickerTrigger } from './PickerTrigger';

const CREATE_TOOLKIT_ROUTE = '/toolkits/create';
const CREATE_MCP_ROUTE = '/mcps/create';

export interface ProjectToolkitPickerContext {
  readonly attachedTools?: readonly PickerToolEntry[] | undefined;
  readonly versionStatus?: string | undefined;
  readonly projectId?: string | undefined;
  readonly yamlJsonObject?: PipelineYamlDocument;
  readonly onMcpLoginClick?: ((mcp: PickerToolEntry | ToolkitListRow) => void) | undefined;
}

export interface ProjectToolkitPickerProps {
  readonly mcp?: boolean | undefined;
  readonly label?: string | undefined;
  readonly tooltip?: ReactNode | undefined;
  readonly value?: string | undefined;
  readonly onSelect?: ((source: PickerToolEntry | ToolkitListRow, toolkitName: string) => void) | undefined;
  readonly onClear?: (() => void) | undefined;
  readonly disabled?: boolean | undefined;
  readonly attachedOnly?: boolean | undefined;
  readonly pickerContext?: ProjectToolkitPickerContext | undefined;
  readonly 'data-testid'?: string | undefined;
}

function getToolkitPickerMessages(mcp: boolean) {
  return {
    searchPlaceholder: mcp
      ? t('pipelines.toolPicker.searchMcps', 'Search MCPs...')
      : t('pipelines.toolPicker.searchToolkits', 'Search toolkits...'),
    emptyMessage: mcp
      ? t('pipelines.toolPicker.emptyMcps', 'No MCPs in this project yet')
      : t('pipelines.toolPicker.emptyToolkits', 'No toolkits in this project yet'),
    noResultsMessage: mcp
      ? t('pipelines.toolPicker.noResultsMcps', 'No MCPs found')
      : t('pipelines.toolPicker.noResultsToolkits', 'No toolkits found'),
  };
}

function getToolkitPickerLabel(mcp: boolean, label: string | undefined): string {
  if (label) return label;
  return mcp
    ? t('pipelines.toolPicker.mcpLabel', 'MCP')
    : t('pipelines.toolPicker.toolkitLabel', 'Toolkit');
}

function resolveSelectedDisplayValue(
  selectedTool: PickerToolEntry | undefined,
  value: string | undefined,
): string | undefined {
  return selectedTool ? getToolkitDisplayName(selectedTool) || value : value;
}

function filterAttachedByMcp(
  attachedTools: readonly PickerToolEntry[] | undefined,
  mcp: boolean,
): PickerToolEntry[] {
  return (attachedTools ?? []).filter(
    (tool) => !isApplicationTool(tool) && isMcpAttachment(tool) === mcp,
  );
}

function suffixTestId(testId: string | undefined, suffix: string): string | undefined {
  return testId ? `${testId}-${suffix}` : undefined;
}

function resolveToolkitPickerContext(
  pickerContext: ProjectToolkitPickerContext | undefined,
  flowYaml: PipelineYamlDocument,
  routeProjectId: string | undefined,
) {
  return {
    yamlJsonObject: pickerContext?.yamlJsonObject ?? flowYaml,
    projectId: pickerContext?.projectId ?? routeProjectId,
    versionStatus: pickerContext?.versionStatus,
    attachedToolsRaw: pickerContext?.attachedTools,
    onMcpLoginClick: pickerContext?.onMcpLoginClick,
  };
}

/**
 * Picks any toolkit (or MCP, when `mcp` is true) of the project for a node.
 */
export const ProjectToolkitPicker = memo(function ProjectToolkitPicker({
  mcp = false,
  label,
  tooltip,
  value,
  onSelect,
  onClear,
  disabled = false,
  attachedOnly = false,
  pickerContext,
  'data-testid': testId,
}: ProjectToolkitPickerProps): ReactNode {
  const flowContext = useContext(FlowEditorContext);
  const routeProjectId = useSelectedProjectId();
  const { yamlJsonObject, projectId, versionStatus, attachedToolsRaw, onMcpLoginClick } =
    resolveToolkitPickerContext(pickerContext, flowContext?.yamlJsonObject, routeProjectId);
  const { toolkitTypeSchemas } = useToolkitTypeSchemas(projectId);
  const { getToolkitNameFromSchema } = useGetToolkitNameFromSchema(toolkitTypeSchemas);
  const isVersionLocked = useIsVersionLocked(versionStatus);
  const resolvedLabel = getToolkitPickerLabel(mcp, label);

  const { anchorEl, isOpen, hasOpened, search, setSearch, query, openMenu, closeMenu } = usePickerMenu();
  const list = useProjectToolkitsList({ mcp, query, skip: attachedOnly || !hasOpened, projectId });
  const openCreatePage = useCreateInNewTab(list.reload);

  const attachedTools = useMemo(
    () => filterAttachedByMcp(attachedToolsRaw, mcp),
    [attachedToolsRaw, mcp],
  );
  const usedNames = useMemo(
    () => getReferencedNames(collectToolReferences(yamlJsonObject), TOOL_REF_KINDS.toolkit),
    [yamlJsonObject],
  );

  const items = useMemo(
    () =>
      buildToolkitPickerItems({
        rows: attachedOnly ? (attachedTools as unknown as readonly ToolkitListRow[]) : list.rows,
        attachedTools,
        usedNames,
        query: search,
        getToolkitName: getToolkitNameFromSchema,
        getLabel: getToolkitDisplayName,
      }),
    [attachedOnly, attachedTools, getToolkitNameFromSchema, list.rows, search, usedNames],
  );

  const selectedTool = useMemo(
    () =>
      value
        ? attachedTools.find((tool) => getAttachmentRefName(tool, getToolkitNameFromSchema) === value)
        : undefined,
    [attachedTools, getToolkitNameFromSchema, value],
  );

  const handleSelect = useCallback(
    (item: PickerItem<PickerToolEntry | ToolkitListRow>) => {
      closeMenu();
      if (item.name !== value) onSelect?.(item.source, item.name);
    },
    [closeMenu, onSelect, value],
  );

  const handleCreate = useCallback(() => {
    closeMenu();
    openCreatePage(mcp ? CREATE_MCP_ROUTE : CREATE_TOOLKIT_ROUTE);
  }, [closeMenu, mcp, openCreatePage]);

  const createActions = useMemo<PickerCreateAction[]>(
    () => [
      {
        key: 'create',
        label: t('pipelines.toolPicker.createNew', 'Create new'),
        onClick: handleCreate,
        testId: suffixTestId(testId, 'create-new'),
      },
    ],
    [handleCreate, testId],
  );

  const groups = useMemo<PickerGroup<PickerItem<PickerToolEntry | ToolkitListRow>>[]>(
    () => [{ key: 'toolkits', title: resolvedLabel, items }],
    [items, resolvedLabel],
  );

  const renderItem = useCallback(
    (item: PickerItem<PickerToolEntry | ToolkitListRow>) => (
      <PickerItemRow
        key={item.key}
        item={item}
        selected={item.name === value}
        onSelect={handleSelect}
        icon={<PickerEntityIcon source={item.source} mcp={mcp} />}
        status={mcp ? <PickerMcpStatus mcp={item.source} onLoginClick={onMcpLoginClick} /> : null}
        data-testid={suffixTestId(testId, 'option')}
      />
    ),
    [handleSelect, mcp, onMcpLoginClick, testId, value],
  );

  const loadState = useMemo(
    () => ({
      isLoading: list.isFetching,
      isError: list.isError,
      onRetry: list.reload,
      onLoadMore: list.onLoadMore,
    }),
    [list.isError, list.isFetching, list.onLoadMore, list.reload],
  );

  const messages = useMemo(() => getToolkitPickerMessages(mcp), [mcp]);
  const displayValue = resolveSelectedDisplayValue(selectedTool, value);
  const menuTestId = suffixTestId(testId, 'menu');

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
          {displayValue}
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
        data-testid={menuTestId}
      />
    </>
  );
});
