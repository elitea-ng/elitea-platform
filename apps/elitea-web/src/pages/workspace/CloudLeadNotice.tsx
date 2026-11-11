/**
 * Above the composer of a lead thread whose lead is a CLOUD agent (ADR-0050 decision 7c/7f, #1929): a message that
 * names no agent goes to the lead, and a cloud agent's turn takes its text only. The host refuses @ files, images,
 * skills, "/" commands and `/compact` for it (`cloud_agent_text_only`); this says so before the send, as the web chat's
 * "To: Name (lead)" says who answers, and names a local agent to address instead.
 *
 * The thread's agents and lead come from the same kept read the send uses (`threadFloorQuery`, #1928).
 */
import Alert from '@mui/material/Alert';
import { useQuery } from '@tanstack/react-query';

import { FLOOR_POLICY, floorOf } from '@elitea/client-core/conversation';
import { t } from '@/shared/i18n';

import type { ThreadAgent } from './useEnsureConversation';
import { threadFloorQuery, type ThreadFloor } from './useSendPrompt';

/** The lead of `floor` when it is a cloud agent of a thread of several agents, and a local agent to address instead. */
export function cloudLeadOf(floor: ThreadFloor | undefined): { lead: ThreadAgent; local: ThreadAgent | undefined } | undefined {
  if (floor === undefined || floor.agents.length < 2) return undefined;
  const policy = floorOf(floor.meta);
  if (policy.policy !== FLOOR_POLICY.Lead) return undefined;
  const lead = floor.agents.find((agent) => agent.participantId === policy.leadParticipantId);
  if (lead === undefined || !lead.cloud) return undefined;
  return { lead, local: floor.agents.find((agent) => !agent.cloud && agent.name !== '') };
}

export interface CloudLeadNoticeProps {
  projectId: number;
  /** The thread; `''` before its first turn (a new thread has no lead). */
  conversationId: string;
  /** A local thread (not the cloud copy, not a pipeline): only there does the host run the message. */
  local: boolean;
}

export function CloudLeadNotice({ projectId, conversationId, local }: CloudLeadNoticeProps): React.JSX.Element | null {
  const enabled = local && conversationId !== '';
  const floor = useQuery({ ...threadFloorQuery(projectId, conversationId), enabled });
  const cloudLead = enabled ? cloudLeadOf(floor.data) : undefined;
  if (cloudLead === undefined) return null;
  const { lead, local: other } = cloudLead;
  const name = lead.name === '' ? t('workspace.cloudLead.unnamed', 'the lead') : lead.name;
  return (
    <Alert severity="info" data-testid="workspace-cloud-lead-notice">
      {t(
        'workspace.cloudLead.text',
        'To: {{name}} (lead). A message that names no agent goes to {{name}}, which runs in the cloud and gets only the text: no @ files, images, skills, / commands or /compact.',
        { name },
      )}
      {other === undefined ? null : ` ${t('workspace.cloudLead.address', 'To use them, address a local agent, for example @{{local}}.', { local: other.name })}`}
    </Alert>
  );
}
