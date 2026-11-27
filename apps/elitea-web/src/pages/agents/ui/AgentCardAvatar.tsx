import { useEffect, useRef, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';

import { AgentAvatar } from '@/features/chat-messages';

/** The rich card's avatar edge (`entityRichCard.styles.ts`'s `AVATAR_SIZE`). */
const SIZE = '3.25rem';

/**
 * An agent's own avatar on its Agents card (issue 3133), through the shared
 * live-avatar renderer: the still at rest, and the idle motion (the pack's
 * idle loop, or the default motion over the still) while the pointer is over
 * the card or focus is inside it. Reduced motion is the renderer's to honour.
 *
 * The card is a `shared/ui` component and knows nothing of avatars, so this
 * listens on the card around it rather than taking a prop the card would
 * have to thread.
 */
export function AgentCardAvatar({ iconUrl, pack }: { readonly iconUrl: string | undefined; readonly pack: unknown }): ReactNode {
  const ref = useRef<HTMLSpanElement>(null);
  const [active, setActive] = useState(false);
  useEffect(() => {
    const card = ref.current?.closest('[data-variant="rich"]');
    if (card === null || card === undefined) return undefined;
    const on = (): void => setActive(true);
    const off = (): void => setActive(false);
    const events: readonly (readonly [string, () => void])[] = [
      ['pointerenter', on],
      ['pointerleave', off],
      ['focusin', on],
      ['focusout', off],
    ];
    for (const [name, handler] of events) card.addEventListener(name, handler);
    return () => {
      for (const [name, handler] of events) card.removeEventListener(name, handler);
    };
  }, []);
  return (
    <Box
      ref={ref}
      component="span"
      data-testid="agent-card-avatar"
      data-active={active ? 'true' : undefined}
      sx={{ display: 'contents' }}
    >
      <AgentAvatar
        iconUrl={iconUrl}
        pack={pack}
        motion={active ? 'idle' : 'still'}
        size={SIZE}
      />
    </Box>
  );
}
