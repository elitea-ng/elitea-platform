import type { ChangeEvent, KeyboardEvent, ReactNode } from 'react';
import { memo, useCallback, useContext, useEffect, useState } from 'react';

import Box from '@mui/material/Box';
import TextField from '@mui/material/TextField';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { PipelineNodeTypes } from '../../lib/flow-editor/constants/flowEditor.constants';
import { NODE_FIELD_KEYS } from '../../lib/flow-editor/constants/nodeFieldInfo.constants';
import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import type { FlowEdge, FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import {
  findRenameRejection,
  renameFlowEdge,
  renameFlowNode,
  renameYamlDocument,
} from '../nodes/BaseNode/NodeCardHeader.rename';
import { NodeFieldHeading } from './NodeConfigSection';

export interface NodeNameFieldProps {
  readonly name: string;
  readonly type: string;
  readonly onRenamed?: ((newName: string) => void) | undefined;
  readonly isLocked?: boolean | undefined;
  readonly toolNames?: readonly string[] | undefined;
}

const rootSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.25rem',
  width: '100%',
};

/**
 * Editable node name input inside `NodeConfigPanel` with `nodeFieldInfo` tooltip
 * and compiler-legal / duplicate-name validation.
 */
export const NodeNameField = memo(function NodeNameField({
  name,
  type,
  onRenamed,
  isLocked = false,
  toolNames = [],
}: NodeNameFieldProps): ReactNode {
  const flowContext = useContext(FlowEditorContext);
  const [draftName, setDraftName] = useState(name);
  const [error, setError] = useState('');

  useEffect(() => {
    setDraftName(name);
    setError('');
  }, [name]);

  const canRename =
    !isLocked &&
    type !== PipelineNodeTypes.Condition &&
    !flowContext?.isRunningPipeline &&
    !flowContext?.disabled;

  const onChange = useCallback(
    (event: ChangeEvent<HTMLInputElement>) => {
      const { value } = event.target;
      setDraftName(value);
      if (!flowContext || value === name) {
        setError('');
        return;
      }
      setError(findRenameRejection(value, name, flowContext.yamlJsonObject, toolNames) ?? '');
    },
    [flowContext, name, toolNames],
  );

  const commit = useCallback(() => {
    if (!flowContext || !canRename || draftName === name) return;
    const rejection = findRenameRejection(draftName, name, flowContext.yamlJsonObject, toolNames);
    if (rejection) {
      setError(rejection);
      return;
    }
    flowContext.setYamlJsonObject(renameYamlDocument(flowContext.yamlJsonObject, name, draftName));
    flowContext.setFlowNodes((prevNodes: FlowNode[]) => prevNodes.map((node) => renameFlowNode(node, name, draftName)));
    flowContext.setFlowEdges((prevEdges: FlowEdge[]) => prevEdges.map((edge) => renameFlowEdge(edge, name, draftName)));
    setError('');
    onRenamed?.(draftName);
  }, [canRename, draftName, flowContext, name, onRenamed, toolNames]);

  const onKeyDown = useCallback((event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'Enter') {
      event.preventDefault();
      (event.target as HTMLInputElement).blur();
    }
  }, []);

  return (
    <Box sx={rootSx}>
      <NodeFieldHeading
        label={t('pipelines.nodeConfigPanel.name', 'Name')}
        fieldKey={NODE_FIELD_KEYS.name}
        required
      />
      <TextField
        value={draftName}
        onChange={onChange}
        onBlur={commit}
        onKeyDown={onKeyDown}
        required
        fullWidth
        size="small"
        variant="outlined"
        autoComplete="off"
        disabled={!canRename}
        error={Boolean(error)}
        helperText={error || undefined}
        slotProps={{
          htmlInput: {
            'aria-label': t('pipelines.nodeConfigPanel.name', 'Name'),
            'data-testid': 'pipeline-node-panel-name-input',
          },
        }}
      />
    </Box>
  );
});
