/**
 * The expanded rail's users row.
 *
 * Split out of `ParticipantsLayout.tsx` for that file's §3.5 length budget,
 * and because the row grew a control: until now it was a strip of avatars and
 * nothing else, so a person could be attached to a conversation (over the
 * REST API, or — since gap G2 was closed — from the panel's own picker) and
 * could never be taken off it again from this panel. Every other participant
 * type has carried a remove control on its card since the rail landed;
 * `DeleteParticipantButton` already resolves the "user" entity word and its
 * confirmation copy, so this is a composition, not a new affordance.
 *
 * The control is revealed by REACT hover state rather than a CSS `:hover`
 * rule, matching `ParticipantItem`'s own `isHovering` — the sibling cards
 * behave that way, and a row that revealed its destructive control by
 * stylesheet alone would also reveal it under a stray pointer during a
 * scroll.
 */
import { memo, useState } from 'react';
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import type { TransformedParticipant } from '../../model/types';

/**
 * `getChatParticipantUniqueId`/`DeleteParticipantButton` are typed against
 * the raw snake_case WIRE row (`Record<string, unknown>`), which is what the
 * rail actually holds; `TransformedParticipant` is this cluster's named
 * description of the same object. One bridge, named, rather than a cast at
 * each of the three call sites.
 */
function asWireRow(participant: TransformedParticipant): Record<string, unknown> {
  return participant as unknown as Record<string, unknown>;
}

/** The same bridge for the callback `DeleteParticipantButton` hands the row back through. */
function asWireHandler(handler: (participant: TransformedParticipant) => void): (participant: Record<string, unknown>) => void {
  return handler as unknown as (participant: Record<string, unknown>) => void;
}
import DeleteParticipantButton from '../ParticipantActions/DeleteParticipantButton';
import { getChatParticipantUniqueId } from '../../lib/helpers';
import { styles } from './participants.styles';
import ParticipantItemRow from './ParticipantItemRow';

export interface UsersRowProps {
  /** The user participants to draw, already capped to the visible count. */
  readonly usersToRender: readonly TransformedParticipant[];
  /** How many more the row is not drawing (`+N`), or `0`. */
  readonly overflowCount: number;
  /** The rail's currently active participant, for the highlight. */
  readonly activeParticipantId?: string | undefined;
  /** Selecting a person — the "@"-mention shortcut the avatars have always had. */
  readonly onSelectParticipant: (participant: TransformedParticipant) => void;
  /** Detaching a person. Omit to render no remove control (a playback rail, or a consumer with no mutation). */
  readonly onDeleteParticipant?: ((participant: TransformedParticipant) => void) | undefined;
  /** When true, the remove control is rendered but refuses (playback). */
  readonly disabledEdit?: boolean | undefined;
}

/** One avatar plus its hover-revealed remove control. */
const UserRowEntry = memo(function UserRowEntry(props: {
  readonly participant: TransformedParticipant;
  readonly isActive: boolean;
  readonly onSelectParticipant: (participant: TransformedParticipant) => void;
  readonly onDeleteParticipant?: ((participant: TransformedParticipant) => void) | undefined;
  readonly disabledEdit?: boolean | undefined;
}): ReactNode {
  const [isHovering, setIsHovering] = useState(false);
  const { participant, isActive, onSelectParticipant, onDeleteParticipant, disabledEdit } = props;

  return (
    <Box
      sx={{ display: 'flex', alignItems: 'center' }}
      onMouseEnter={() => setIsHovering(true)}
      onMouseLeave={() => setIsHovering(false)}
    >
      <ParticipantItemRow participant={participant} isActive={isActive} onClickItem={onSelectParticipant} />
      {onDeleteParticipant && isHovering && (
        <DeleteParticipantButton
          participant={asWireRow(participant)}
          onDelete={asWireHandler(onDeleteParticipant)}
          disabled={disabledEdit === true}
        />
      )}
    </Box>
  );
});

export const UsersRow = memo(function UsersRow({
  usersToRender,
  overflowCount,
  activeParticipantId,
  onSelectParticipant,
  onDeleteParticipant,
  disabledEdit,
}: UsersRowProps): ReactNode {
  return (
    <Box sx={styles.usersRow()} data-testid="users-section">
      <Box sx={styles.usersDisplay}>
        {usersToRender.map((participant) => (
          <UserRowEntry
            key={getChatParticipantUniqueId(asWireRow(participant))}
            participant={participant}
            isActive={activeParticipantId === getChatParticipantUniqueId(asWireRow(participant))}
            onSelectParticipant={onSelectParticipant}
            onDeleteParticipant={onDeleteParticipant}
            disabledEdit={disabledEdit}
          />
        ))}
        {overflowCount > 0 && (
          <Typography variant="bodySmall" sx={styles.usersOverflow}>
            {`+${String(overflowCount)}`}
          </Typography>
        )}
      </Box>
    </Box>
  );
});
