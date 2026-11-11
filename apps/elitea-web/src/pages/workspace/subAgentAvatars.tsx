/**
 * The avatars the workspace pages give a transcript's sub-agent lanes
 * (`TurnTranscript`'s `subAgentAvatar`): `AgentAvatar` lives in
 * `features/chat-messages`, which `features/workspace` may not import.
 */
import { AgentAvatar } from '@/features/chat-messages';

import type { SubAgentLane } from './cloudTranscriptModel';

/**
 * A cloud child: the dashed ring of an ephemeral participant (ADR-0050
 * decision 7e). Its profile's avatar key names no image yet, so the ring
 * holds the brand mark.
 */
export function TemporaryAvatar(_props: { lane: SubAgentLane }): React.JSX.Element {
  return <AgentAvatar ephemeral size="1rem" />;
}

/**
 * A local child whose spawn profile named an avatar (`subagent_started`,
 * #1910): an avatar on the lane, as the cloud lane draws one. The key
 * (`default:<name>`) names no image yet, on either lane, so this draws the
 * brand mark without the cloud's dashed ring; a profile with none draws
 * nothing, as before.
 */
export function ProfileAvatar({ lane }: { lane: SubAgentLane }): React.JSX.Element | null {
  return lane.avatar === undefined ? null : <AgentAvatar size="1rem" />;
}
