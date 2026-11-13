import type { ReactNode } from 'react';
import { useCallback, useContext, useMemo } from 'react';

import { FlowEditorContext } from '../../lib/flow-editor/flowEditorContext';
import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import * as FlowEditorHelpers from '../../lib/flow-editor/helpers/flowEditor.helpers';
import type { YamlPipelineNode } from '../../lib/flow-editor/helpers/pipelineFlow.types';
import type { FlowEdge } from '../../lib/flow-editor/reactFlowTypes';
import { useNodeOptions } from '../../lib/flow-editor/hooks/useNodeOptions';
import { PipelineMultiSelect, type PipelineMultiSelectOption } from './PipelineMultiSelect';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * select/RouteSelect.jsx` (unit A2h). See `InputSelect.tsx`'s doc comment
 * for the `FlowEditorContext`-source and `PipelineMultiSelect` deviations
 * (identical rationale, not repeated here).
 */
export interface RouteSelectProps {
  readonly id: string;
  readonly label?: string;
  readonly fieldName?: string;
  readonly nodesFilter?: (node: YamlPipelineNode) => boolean;
  readonly addEndNode?: boolean;
  readonly disabled?: boolean | undefined;
}

const defaultNodesFilter = (): boolean => true;

export function RouteSelect(props: RouteSelectProps): ReactNode {
  const {
    id,
    label = 'Route',
    fieldName = 'routes',
    nodesFilter = defaultNodesFilter,
    addEndNode,
    disabled = false,
  } = props;

  const context = useContext(FlowEditorContext);
  const setFlowEdges = context?.setFlowEdges;
  const setYamlJsonObject = context?.setYamlJsonObject;
  const yamlJsonObject = context?.yamlJsonObject;

  const yamlNode = useMemo(
    () => yamlJsonObject?.nodes?.find(node => node.id === id),
    [id, yamlJsonObject?.nodes],
  );
  const nodeOptions = useNodeOptions(nodesFilter, addEndNode);

  const onChangeInput = useCallback(
    (newValue: string[]) => {
      if (!setYamlJsonObject) return;
      FlowEditorHelpers.updateYamlNode(id, fieldName, newValue, yamlJsonObject, setYamlJsonObject);
      // A context with no canvas (no edge setter) still gets the YAML write — the chip ✕ used to work there.
      if (!setFlowEdges) return;
      const routerHandle = `${FlowEditorConstants.ROUTER_HANDLE_ID_SUFFIX}_${fieldName}`;

      setFlowEdges(prevEdges => {
        if (newValue.length === 0) {
          return prevEdges.filter(edge => edge.source !== id || edge.sourceHandle !== routerHandle);
        }

        const filteredEdges = prevEdges.filter(
          edge => edge.source !== id || edge.sourceHandle !== routerHandle || newValue.includes(edge.target),
        );
        const routesEdges = filteredEdges.filter(
          edge => edge.source === id && edge.sourceHandle === routerHandle,
        );

        if (routesEdges.length !== newValue.length) {
          const newEdges: FlowEdge[] = newValue
            .filter(value => !routesEdges.some(edge => edge.target === value))
            .map(value => ({
              id: `${FlowEditorConstants.EDGE_PREFIX}${id}---${value}`,
              source: id,
              sourceHandle: routerHandle,
              target: value,
              type: 'custom',
              data: yamlJsonObject?.interrupt_before?.includes(value) ? { label: 'interrupt' } : {},
            }));
          return [...filteredEdges, ...newEdges];
        }
        return filteredEdges;
      });
    },
    [id, fieldName, setFlowEdges, setYamlJsonObject, yamlJsonObject],
  );

  const routesFromNode = useMemo(() => {
    const raw = yamlNode ? yamlNode[fieldName] : undefined;
    return Array.isArray(raw) ? (raw as string[]) : [];
  }, [fieldName, yamlNode]);

  const realNodeOptions = useMemo<PipelineMultiSelectOption[]>(() => {
    const optionsNotInState: PipelineMultiSelectOption[] = routesFromNode
      .filter(item => !nodeOptions.find(option => option.value === item))
      .map(item => ({ label: item, value: item, canDelete: true, tooltip: 'Not in state' }));
    return [...optionsNotInState, ...nodeOptions];
  }, [routesFromNode, nodeOptions]);

  // Same path as deselecting in the menu: the YAML route AND its canvas edge go together.
  // Writing only the YAML (as this did) left an orphaned connection line behind (#2006).
  const onDeleteOption = useCallback(
    (value: string) => {
      onChangeInput(routesFromNode.filter(item => item !== value));
    },
    [onChangeInput, routesFromNode],
  );

  return (
    <PipelineMultiSelect
      label={label}
      value={routesFromNode}
      onValueChange={onChangeInput}
      options={realNodeOptions}
      disabled={disabled}
      className="nopan nodrag nowheel"
      onDeleteOption={onDeleteOption}
    />
  );
}
