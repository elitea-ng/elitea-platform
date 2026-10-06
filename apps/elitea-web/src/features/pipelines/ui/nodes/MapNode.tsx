import type { ReactNode } from 'react';
import { memo } from 'react';
import type { NodeProps } from '@xyflow/react';
import type { FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import { ExtensionNodeShell } from '../settings/graph-extensions/ExtensionNodeShell';
import { MapSettings } from '../settings/graph-extensions/MapSettings';

export const MapNode = memo(function MapNode(props: NodeProps<FlowNode>): ReactNode {
  return <ExtensionNodeShell {...props} renderSettings={(settings) => <MapSettings {...settings} />} />;
});
