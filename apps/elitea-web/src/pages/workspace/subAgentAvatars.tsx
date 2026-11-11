/**
 * The avatars the workspace pages give a transcript's sub-agent lanes
 * (`TurnTranscript`'s `subAgentAvatar`): `AgentAvatar` lives in
 * `features/chat-messages`, which `features/workspace` may not import.
 */
import { AgentAvatar } from '@/features/chat-messages';

import type { SubAgentLane } from './cloudTranscriptModel';

/** A cloud child: a temporary agent's dashed ring (ADR-0050 decision 7e). */
export function TemporaryAvatar(_props: { lane: SubAgentLane }): React.JSX.Element {
  return <AgentAvatar ephemeral size="1rem" />;
}

/**
 * A local child whose spawn profile named an avatar (`subagent_started`,
 * #1910): the profile's avatar, as the cloud lane draws one. The key is
 * display only (`default:<name>`), so it draws the default pack; a profile
 * with none draws nothing, as before.
 */
export function ProfileAvatar({ lane }: { lane: SubAgentLane }): React.JSX.Element | null {
  return lane.avatar === undefined ? null : <AgentAvatar size="1rem" />;
}
