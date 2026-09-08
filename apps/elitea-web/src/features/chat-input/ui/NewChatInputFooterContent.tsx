import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import type { Participant } from '@/entities/participant';

import { AgentEditorPanel } from './AgentEditorPanel';
import type { AgentEditorPanelProps } from './AgentEditorPanel.types';
import type { NewChatInputSlots } from './NewChatInput.types';

/**
 * The footer's button row, factored out of `NewChatInput.tsx` for the same
 * §3.5 complexity-budget reason as `UserInput.tsx`'s own sub-components.
 * Baseline: `NewChatInput.jsx`'s `slots.footer` JSX block.
 *
 * `slots.attachmentButton` is rendered unconditionally (no `fromTheChat`/
 * `hideAttachments` gate on this side) — those two baseline conditions
 * decide WHICH C6 component flavour to build (`PlusChatButton` vs. plain
 * `ChatButton.AttachmentButton` vs. nothing); now that the whole area is
 * one injected `ReactNode` slot, that decision belongs entirely to
 * whatever composition-root unit builds the slot's content (passing
 * `undefined` is how it opts out).
 *
 * `slots.internalToolsConfig`, by contrast, IS gated here on `isAgentsPage`
 * (baseline: `!isAgentsPage && !fromTheChat`) — unlike `fromTheChat`,
 * `isAgentsPage` is already data this cluster owns end-to-end (threaded
 * through `agentEditor.isAgentsPage` and used for the `AgentEditorPanel`/
 * `modelSelector` branch below too), so there is no reason to make the
 * composition root re-derive a rule this component can already enforce
 * directly. The composition root remains responsible for additionally
 * withholding this slot when `fromTheChat` is set (passing `undefined`),
 * exactly like `attachmentButton`.
 * `AgentEditorPanel` vs. `slots.modelSelector` is the one branch THIS
 * cluster still owns — see this component's own `isAgentsPage` prop.
 *
 * `slots.clearChat` is rendered unconditionally, like `attachmentButton` and
 * unlike `internalToolsConfig`: which surfaces offer a clear-history control
 * is not a rule this cluster can state (the pipeline editor already renders
 * one of its own, outside the composer), so the composition root decides by
 * supplying or withholding the node.
 */
export interface NewChatInputFooterContentProps {
  readonly slots: NewChatInputSlots;
  readonly isAgentsPage: boolean;
  readonly activeParticipant: Participant | undefined;
  readonly agentEditorProps: AgentEditorPanelProps;
}

function isApplicationOrPipeline(participant: Participant | undefined): boolean {
  return participant?.entityName === 'application' || participant?.entityName === 'pipeline';
}

export function NewChatInputFooterContent(props: NewChatInputFooterContentProps): ReactNode {
  const { slots, isAgentsPage, activeParticipant, agentEditorProps } = props;

  const showAgentEditorPanel = isApplicationOrPipeline(activeParticipant) && !isAgentsPage;
  const showModelSelector = isAgentsPage || !activeParticipant;

  return (
    <Box sx={rowSx}>
      <Box sx={leftSx}>
        {slots.attachmentButton}
        {!isAgentsPage && slots.internalToolsConfig}
        {slots.clearChat}
      </Box>
      <Box sx={rightSx}>
        {showAgentEditorPanel && <AgentEditorPanel {...agentEditorProps} />}
        {!showAgentEditorPanel && showModelSelector && slots.modelSelector}
        {slots.voiceButton}
      </Box>
    </Box>
  );
}

const rowSx: SxProps<Theme> = {
  flex: 1,
  display: 'flex',
  alignItems: 'center',
  gap: { xs: '.125rem', sm: '.5rem' },
  justifyContent: 'space-between',
  maxWidth: '100%',
  overflow: 'hidden',
};

const leftSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  gap: { xs: '.25rem', sm: '.5rem' },
  flexShrink: 0,
};

const rightSx: SxProps<Theme> = {
  flex: 1,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'flex-end',
  gap: { xs: '.25rem', sm: '.5rem' },
  minWidth: 0,
  flexShrink: 1,
  maxWidth: '100%',
  overflow: 'hidden',
};
