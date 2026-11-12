/**
 * The agent editor's live test chat: the right half of the page (#1974, #1980).
 *
 * The baseline's agent page is a two-panel grid whose right half is a chat
 * with the agent being edited. This app shipped the form alone, and the only
 * way to talk to the agent was a button that filed an ordinary conversation in
 * the Chats sidebar. This pane is the SSE-era answer: the real
 * `widgets/chat-box` `ChatBox`, bound to an `editor_test` conversation that
 * belongs to the agent and stays out of the Chats list
 * (`../lib/useAgentTestConversation.ts`).
 *
 * The conversation is created when the author first uses the composer, and
 * restored (with its messages) when the page is re-entered. `isAgentsPage` is the flag the pipeline editor's pane sets too: it keeps
 * `ChatBox` from adding the ad-hoc model participant, because the turn is
 * addressed to the agent.
 *
 * The chat runs the SAVED version, which is what the participant pins. An
 * unsaved form edit is therefore not part of a test run; the pane says so
 * rather than closing, so a half-edited agent can still be asked a question.
 */
import { useCallback, useEffect, useMemo, useRef } from 'react';
import type { ReactNode, SyntheticEvent } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { applicationServerErrorMessage } from '@/features/agents';
import { t } from '@/shared/i18n';
import { ChatBox, type ChatBoxHandle } from '@/widgets/chat-box';

import { useAgentTestConversation, type AgentTestIdentity } from '../lib/useAgentTestConversation';

export interface AgentTestChatProps {
  readonly identity: AgentTestIdentity;
  /** The signed-in user, resolved once by the page. */
  readonly user: { readonly id?: string; readonly name?: string; readonly avatar?: string } | undefined;
  /** The form holds edits the saved version does not have yet. */
  readonly isDirty: boolean;
  /** The version's conversation starters, shown on the empty transcript. */
  readonly conversationStarters?: readonly string[] | undefined;
}

const paneSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  minHeight: 0,
  height: '100%',
  border: `1px solid ${theme.vars.palette.border.lines}`,
  borderRadius: theme.vars.shape.radiusLg,
  overflow: 'hidden',
});
const headerSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '0.5rem', padding: '0.5rem 0.75rem' };
const noticeSx: SxProps<Theme> = { paddingX: '0.75rem', paddingBottom: '0.5rem' };
const NO_STARTERS: readonly string[] = [];
const editorTestExtensions = { editorTest: {} };
/** The pane's header and the model picker with its menu: touching them is not talking to the agent. */
const NOT_A_CHAT_INTERACTION = '[data-testid="agent-test-chat-header"], [data-testid^="model-selector"], [role="listbox"], [role="option"], [role="menu"], [role="menuitem"], [role="dialog"], [aria-hidden="true"]';

export function AgentTestChat({ identity, user, isDirty, conversationStarters = NO_STARTERS }: AgentTestChatProps): ReactNode {
  const test = useAgentTestConversation(identity);
  const chatBoxRef = useRef<ChatBoxHandle | null>(null);
  const { ensure, clear, scopeKey } = test;

  const handleClear = useCallback(() => {
    chatBoxRef.current?.stopAll();
    clear();
  }, [clear]);

  // Moving to another agent or version stops whatever is streaming; the hook remounts the transcript.
  useEffect(() => {
    chatBoxRef.current?.stopAll();
  }, [scopeKey]);

  const ensureOnChatInteraction = useCallback(
    (event: SyntheticEvent) => {
      const target = event.target;
      if (target instanceof Element && target.closest(NOT_A_CHAT_INTERACTION) !== null) return;
      ensure();
    },
    [ensure],
  );

  const participant = useMemo(() => ({ active: test.activeParticipant }), [test.activeParticipant]);
  const conversation = useMemo(
    () => ({ ...(test.conversation ? { active: test.conversation } : {}), isLoading: test.isCreating }),
    [test.conversation, test.isCreating],
  );

  return (
    <Box
      data-testid="agent-test-chat"
      sx={paneSx}
      // The conversation is created on the first interaction with the
      // composer, in the capture phase so the bootstrap is already in flight
      // when the composer's own handlers run. Clear chat and the model picker
      // (and its menu, which React bubbles through its portal) are not
      // interactions with the chat and create nothing. `ensure` is idempotent,
      // and a no-op until the version has loaded, so the first keystroke
      // retries a click that came too early.
      onPointerDownCapture={ensureOnChatInteraction}
      onFocusCapture={ensureOnChatInteraction}
      onKeyDownCapture={ensureOnChatInteraction}
    >
      <Box
        sx={headerSx}
        data-testid="agent-test-chat-header"
      >
        <Typography
          variant="subtitle"
          component="h2"
        >
          {t('pages.agents.testChat.title', 'Chat with this agent')}
        </Typography>
        <Button
          size="small"
          data-testid="agent-test-chat-clear"
          disabled={test.conversation === undefined}
          onClick={handleClear}
        >
          {t('pages.agents.testChat.clear', 'Clear chat')}
        </Button>
      </Box>
      {isDirty && (
        <Typography
          variant="bodySmall"
          color="text.secondary"
          data-testid="agent-test-chat-dirty"
          sx={noticeSx}
        >
          {t('pages.agents.testChat.unsaved', 'This chat runs the saved version. Save to try your latest changes.')}
        </Typography>
      )}
      {test.hasFailed && (
        <Typography
          role="alert"
          variant="bodySmall"
          data-testid="agent-test-chat-error"
          sx={noticeSx}
        >
          {applicationServerErrorMessage(
            test.failure,
            t('pages.agents.testChat.error', 'Could not start a chat with this agent. Click the message box to try again.'),
          )}
        </Typography>
      )}
      <Box sx={{ display: 'flex', flexDirection: 'column', flex: 1, minHeight: 0 }}>
        <ChatBox
          key={test.generation}
          ref={chatBoxRef}
          conversation={conversation}
          {...(identity.projectId !== undefined ? { projectId: identity.projectId } : {})}
          {...(user ? { user } : {})}
          participant={participant}
          conversationStarters={conversationStarters}
          isAgentsPage
          // The same flag the pipeline pane sets: the composer, stop, unattended-grant and transport paths treat this as an editor_test conversation.
          extensions={editorTestExtensions}
        />
      </Box>
    </Box>
  );
}
