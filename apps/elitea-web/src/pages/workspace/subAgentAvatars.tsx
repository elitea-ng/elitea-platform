/**
 * The avatars the workspace pages give a transcript's sub-agent lanes
 * (`TurnTranscript`'s `subAgentAvatar`): `AgentAvatar` lives in
 * `features/chat-messages`, which `features/workspace` may not import.
 */
import { AgentAvatar } from '@/features/chat-messages';

import type { SubAgentLane } from './cloudTranscriptModel';

/**
 * A cloud child: the dashed ring of an ephemeral participant (ADR-0050
 * decision 7e). The ring holds the image of its profile's avatar key
 * (`default:<name>`); no key or an unknown one holds the brand mark.
 */
export function TemporaryAvatar({ lane }: { lane: SubAgentLane }): React.JSX.Element {
  return <AgentAvatar ephemeral avatarKey={lane.avatar} size="1rem" />;
}

/**
 * A local child whose spawn profile named an avatar (`subagent_started`,
 * #1910): an avatar on the lane, as the cloud lane draws one. The key
 * (`default:<name>`) draws its image; an unknown key draws the brand mark,
 * without the cloud's dashed ring; a profile with none draws nothing.
 */
export function ProfileAvatar({ lane }: { lane: SubAgentLane }): React.JSX.Element | null {
  return lane.avatar === undefined ? null : <AgentAvatar avatarKey={lane.avatar} size="1rem" />;
}
