import type { ReactNode } from 'react';
import type { ToolkitTestAuthorization } from '../../api/toolkitTestAuthorization';

export interface ToolkitTestAuthorizationRenderProps {
  readonly projectId: string;
  readonly challenge: ToolkitTestAuthorization;
  readonly onAuthorized: (reference: string) => Promise<void>;
  readonly onSkip: () => void;
}

export type ToolkitTestAuthorizationRenderer = (props: ToolkitTestAuthorizationRenderProps) => ReactNode;
