import type { ReactNode } from 'react';
import { memo } from 'react';

import type { NodeProps } from '@xyflow/react';

import type { FlowNode } from '../../lib/flow-editor/reactFlowTypes';
import { ExtensionNodeShell } from '../settings/graph-extensions/ExtensionNodeShell';
import { HttpActionSettings } from '../settings/http-action/HttpActionSettings';

export const HttpNode = memo(function HttpNode(props: NodeProps<FlowNode>): ReactNode {
  return (
    <ExtensionNodeShell
      {...props}
      renderSettings={(settings) => <HttpActionSettings {...settings} />}
    />
  );
});
