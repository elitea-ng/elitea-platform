/**
 * The change-request dialog the Evaluation tab's Enhance with AI opens for the
 * draft version it created. The page composes it because `agent-evaluation`
 * may not import `agent-lifecycle` (see `requestPublishSlot` in that slice).
 */
import type { ReactNode } from 'react';

import { RequestPublishDialog, useAgentRegistry } from '@/features/agent-lifecycle';

interface EvaluationRequestPublishProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly versionId: number;
  readonly onClose: () => void;
}

export function EvaluationRequestPublish({
  projectId,
  applicationId,
  versionId,
  onClose,
}: EvaluationRequestPublishProps): ReactNode {
  const registry = useAgentRegistry(projectId, applicationId);
  return (
    <RequestPublishDialog
      open
      projectId={projectId}
      applicationId={applicationId}
      versionId={versionId}
      riskTier={registry.data?.risk_tier}
      onClose={onClose}
    />
  );
}
