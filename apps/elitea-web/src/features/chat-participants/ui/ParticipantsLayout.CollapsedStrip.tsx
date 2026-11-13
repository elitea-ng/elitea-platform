/**
 * Split out of `ParticipantsLayout.tsx` to stay under the §3.5 file-length
 * budget — `CollapsedParticipantsStrip`, the collapsed icon-strip
 * (adversarial review C5-wrapper #4): one dropdown per populated type,
 * mirroring old-app `CollapsedPerticapantsList.jsx`. Forwards
 * `activeParticipantId`/`disabledEdit`/`onDeleteParticipant`/
 * `onEditParticipant` — all already available via `sections`/`actions` —
 * to `CollapsedParticipantsDropdown`, which accepts them; `maxVisible`
 * was dropped, no longer a prop on that component.
 */
import Box from '@mui/material/Box';

import type { TransformedParticipant } from '../model/types';
import CollapsedParticipantsDropdown from './CollapsedParticipants/CollapsedParticipantsDropdown';
import type { ParticipantsProps } from './Participants.types';

interface UserDisplay {
  usersToDisplay: TransformedParticipant[];
  /** Every user, uncapped — the rail slices it itself so the people behind "+N" can be listed (#1964). */
  allUsers?: TransformedParticipant[];
  hasOverflow: boolean;
  visibleCount: number;
  maxVisibleUsers: number;
}

interface SectionConfig {
  sections: Array<{ key: string; type: string; participants: TransformedParticipant[]; entityType: string }>;
  activeParticipantId?: string;
  disabledEdit?: boolean;
  disabledAdd?: boolean;
  selectedManager?: string;
  newConversationSelectedManager?: string;
}

interface ParticipantActions {
  onSelectParticipant?: (p: TransformedParticipant) => void;
  onDeleteParticipant?: (p: TransformedParticipant) => void;
  onEditParticipant?: (p: TransformedParticipant) => void;
  onUpdateParticipant?: (p: TransformedParticipant) => void;
  editingToolkit?: string;
  resolveToolkitIcon?: ParticipantsProps['resolveToolkitIcon'];
}

export interface CollapsedParticipantsStripProps {
  users: UserDisplay;
  sections: SectionConfig;
  actions: ParticipantActions;
}

/**
 * `CollapsedParticipantsDropdown` predates this feature's typed
 * `TransformedParticipant` and still declares its props against the
 * generic `Record<string, unknown>` wire shape (same boundary this file's
 * sibling `ParticipantsLayout.tsx` bridges via a cast for `ParticipantItemRow`).
 * `TransformedParticipant` objects are plain objects at runtime, so this is
 * a safe type-level bridge, not a behavior change.
 */
type WireParticipant = Record<string, unknown>;
function asWireParticipants(participants: readonly TransformedParticipant[]): WireParticipant[] {
  return participants as unknown as WireParticipant[];
}
function asWireHandler(handler: ((p: TransformedParticipant) => void) | undefined): ((p: WireParticipant) => void) | undefined {
  return handler as unknown as ((p: WireParticipant) => void) | undefined;
}

/** `exactOptionalPropertyTypes`-safe optional-field spread — same pattern `widgets/chat-box`'s `optField` establishes, reimplemented locally since `features/` may not import `widgets/`. */
function optField<K extends string, V>(key: K, value: V | undefined): { readonly [P in K]?: V } {
  return (value !== undefined ? { [key]: value } : {}) as { readonly [P in K]?: V };
}

export function CollapsedParticipantsStrip({ users, sections, actions }: CollapsedParticipantsStripProps) {
  const onItemClick = asWireHandler(actions.onSelectParticipant);
  const onDeleteParticipant = asWireHandler(actions.onDeleteParticipant);
  const onEditParticipant = asWireHandler(actions.onEditParticipant);
  const sharedProps = {
    ...optField('activeParticipantId', sections.activeParticipantId),
    ...optField('disabledEdit', sections.disabledEdit),
    ...optField('onItemClick', onItemClick),
    ...optField('onDeleteParticipant', onDeleteParticipant),
    ...optField('onEditParticipant', onEditParticipant),
  };

  return (
    <Box
      sx={{ display: 'flex', flexDirection: 'column', alignItems: 'center', gap: '.5rem', width: '100%' }}
      data-testid="collapsed-participants-strip"
    >
      {users.visibleCount > 0 && (
        <CollapsedParticipantsDropdown
          participants={asWireParticipants(users.usersToDisplay)}
          {...sharedProps}
        />
      )}
      {sections.sections.map(({ key, participants: group }) => (
        <CollapsedParticipantsDropdown
          key={key}
          participants={asWireParticipants(group)}
          {...sharedProps}
        />
      ))}
    </Box>
  );
}

CollapsedParticipantsStrip.displayName = 'CollapsedParticipantsStrip';
