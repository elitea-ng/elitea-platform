import type { ReactNode } from 'react';
import { memo, useCallback, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { CodeIcon } from '@/shared/ui/icons/code-icon';
import { FormIcon } from '@/shared/ui/icons/form-icon';
import { TabGroupButton, type TabGroupButtonItem } from '@/shared/ui/TabGroupButton';

import { CommonInterruptSettings } from '../settings/CommonInterruptSettings';
import { NodeSettings } from '../settings/NodeSettings';
import { NODE_CONFIG_DRAWER_WIDTH_REM } from './NodeConfigDrawer';
import { NodeConfigFooter } from './NodeConfigFooter';
import { NodeConfigPanelHeader } from './NodeConfigPanelHeader';
import { NodeConfigSection } from './NodeConfigSection';
import { NodeYamlView } from './NodeYamlView';

export type NodeConfigViewMode = 'flow' | 'yaml';

export interface NodeConfigPanelSlots {
  /** Primary node configuration fields (e.g. inputs, prompt, model). */
  readonly basic?: ReactNode;
  /** Composition slot for Wave U1 `tool-picker` (`ProjectAgentPicker`, `ProjectToolkitPicker`, `PipelineToolsSection`). */
  readonly toolPicker?: ReactNode;
  /** Optional custom interrupt controls; defaults to `CommonInterruptSettings` (Q6) inside Advanced. */
  readonly interruptSettings?: ReactNode;
  /** Optional custom settings controls; defaults to `NodeSettings` inside Advanced. */
  readonly nodeSettings?: ReactNode;
  /** Extra node-specific controls placed inside the Advanced accordion. */
  readonly advanced?: ReactNode;
  /** Optional admission issues / deprecation banner placed above the Basic section. */
  readonly banner?: ReactNode;
}

export interface NodeConfigPanelYamlState {
  readonly code: string;
  readonly onChangeCode: (nextCode: string) => void;
  readonly error?: string | null | undefined;
}

export interface NodeConfigPanelActions {
  readonly onClose: () => void;
  readonly onMakeEntrypoint?: ((id: string) => void) | undefined;
  readonly onDuplicate?: ((id: string) => void) | undefined;
  readonly onDelete?: ((id: string) => void) | undefined;
}

export interface NodeConfigPanelProps {
  readonly id: string;
  readonly type: string;
  readonly isEntrypoint?: boolean | undefined;
  readonly canMakeEntrypoint?: boolean | undefined;
  readonly canDuplicate?: boolean | undefined;
  readonly disabled?: boolean | undefined;
  readonly showStructuredOutput?: boolean | undefined;
  readonly defaultView?: NodeConfigViewMode | undefined;
  readonly yaml: NodeConfigPanelYamlState;
  readonly actions: NodeConfigPanelActions;
  readonly slots?: NodeConfigPanelSlots | undefined;
}

const rootSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexDirection: 'column',
  width: NODE_CONFIG_DRAWER_WIDTH_REM,
  maxWidth: '100%',
  height: '100%',
  boxSizing: 'border-box',
  backgroundColor: theme.vars.palette.background.paper,
  borderLeft: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  overflow: 'hidden',
});

const bodySx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '1rem',
  padding: '1rem 1.5rem',
  flex: 1,
  minHeight: 0,
  overflowY: 'auto',
};

const viewToggleItemSx: SxProps<Theme> = {
  width: '2rem',
  minWidth: '2rem',
  height: '1.75rem',
};

const toggleIconStyle = { width: '1rem', height: '1rem' };

interface NodeConfigFlowBodyProps {
  readonly id: string;
  readonly type: string;
  readonly disabled: boolean;
  readonly showStructuredOutput: boolean;
  readonly slots: NodeConfigPanelSlots | undefined;
}

function NodeConfigFlowBody({
  id,
  type,
  disabled,
  showStructuredOutput,
  slots,
}: NodeConfigFlowBodyProps): ReactNode {
  return (
    <>
      {slots?.banner}
      <NodeConfigSection section="basic">
        {slots?.basic}
        {slots?.toolPicker}
      </NodeConfigSection>
      <NodeConfigSection section="advanced" defaultExpanded={false}>
        {slots?.interruptSettings ?? (
          <CommonInterruptSettings
            id={id}
            type={type}
            disabled={disabled}
            showStructuredOutput={showStructuredOutput}
          />
        )}
        {slots?.nodeSettings ?? <NodeSettings id={id} type={type} disabled={disabled} />}
        {slots?.advanced}
      </NodeConfigSection>
    </>
  );
}

/**
 * 400px Wave U2 node configuration panel with Flow/YAML view toggle, Basic & Advanced sections
 * (hosting Q6 interrupt switches and `NodeSettings`), Wave U1 `tool-picker` slot, and Q2 footer.
 */
export const NodeConfigPanel = memo(function NodeConfigPanel(props: NodeConfigPanelProps): ReactNode {
  const { id, type, yaml, actions, slots } = props;
  const disabled = props.disabled ?? false;
  const [view, setView] = useState<NodeConfigViewMode>(props.defaultView ?? 'flow');

  const viewItems = useMemo<TabGroupButtonItem[]>(
    () => [
      {
        value: 'flow',
        icon: <FormIcon style={toggleIconStyle} />,
        tooltip: t('pipelines.nodeConfigPanel.flowView', 'Flow view'),
      },
      {
        value: 'yaml',
        icon: <CodeIcon style={toggleIconStyle} />,
        tooltip: t('pipelines.nodeConfigPanel.yamlView', 'YAML view'),
      },
    ],
    [],
  );

  const handleViewChange = useCallback((next: string) => {
    if (next === 'flow' || next === 'yaml') setView(next);
  }, []);

  return (
    <Box sx={rootSx} className="nokey" data-testid="pipeline-node-panel" data-node-id={id}>
      <NodeConfigPanelHeader type={type} onClose={actions.onClose} />
      <Box sx={bodySx}>
        <Box data-testid="pipeline-node-panel-view-toggle">
          <TabGroupButton
            items={viewItems}
            value={view}
            onChange={handleViewChange}
            ariaLabel={t('pipelines.nodeConfigPanel.viewMode', 'Node configuration view mode')}
            itemSx={viewToggleItemSx}
          />
        </Box>
        {view === 'yaml' ? (
          <NodeYamlView
            code={yaml.code}
            onChangeCode={yaml.onChangeCode}
            error={yaml.error}
            disabled={disabled}
          />
        ) : (
          <NodeConfigFlowBody
            id={id}
            type={type}
            disabled={disabled}
            showStructuredOutput={props.showStructuredOutput ?? false}
            slots={slots}
          />
        )}
      </Box>
      <NodeConfigFooter
        id={id}
        type={type}
        isEntrypoint={props.isEntrypoint ?? false}
        canMakeEntrypoint={props.canMakeEntrypoint}
        canDuplicate={props.canDuplicate}
        disabled={disabled}
        onMakeEntrypoint={actions.onMakeEntrypoint}
        onDuplicate={actions.onDuplicate}
        onDelete={actions.onDelete}
      />
    </Box>
  );
});
