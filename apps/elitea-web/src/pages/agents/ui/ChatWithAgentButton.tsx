import type { ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import Button from '@mui/material/Button';

import { useNavigate } from '@tanstack/react-router';

import { conversationApi } from '@/entities/conversation';
import { useAddParticipantMutation } from '@/entities/participant';
import type { ApplicationVersionDetail, SocialAuthorProfile } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';

/** `ChatBox` truncates a conversation name to 50 characters; match it so the sidebar entry reads the same everywhere. */
const MAX_CONVERSATION_NAME = 50;

export interface ChatWithAgentButtonProps {
  readonly projectId: string | undefined;
  /** The agent's id, as the route carries it. */
  readonly applicationId: string | undefined;
  readonly name: string | undefined;
  /** The version the chat runs — the participant pins it, see the mapping note below. */
  readonly activeVersion: ApplicationVersionDetail | undefined;
  /** Called with the failure, so the caller can show the server's own refusal (e.g. FU-244 composition). */
  readonly onError: (error?: unknown) => void;
}

/**
 * "Chat" — the way to actually TALK to the agent being edited.
 *
 * The baseline's agent page is a two-panel grid whose right half is a live
 * test chat (`apps/elitea-ui/src/pages/Applications/Components/Applications/
 * ConfigurationTab.jsx` — `LeftGridItem` form, `RightGridItem` `ChatBox`).
 * That pane's port (`features/agents/ui/ConfigurationTab.tsx` +
 * the `useApplicationChat` hook family) exists but is still socket-era and
 * mounted by nothing, so this page shipped with the form and an empty right
 * half: an agent could be authored, and there was no way to speak to it.
 *
 * This button is the SSE-era answer, built from the parts a live browser run
 * already proved out end to end: it creates a conversation, attaches the
 * agent as a participant, and lands in the real chat surface — the same
 * three steps the chat page's "+" menu performs, addressed from the agent's
 * side instead.
 *
 * The participant mapping is copied from what the resolver actually joins on
 * (`services/elitea-main/internal/db/queries/agent_chat.sql`):
 *  - `entity_settings.version_id` is LOAD-BEARING — the version join reads
 *    exactly that key, and a participant without it makes every turn answer
 *    422 "This agent turn requires the current execution path." (the defect
 *    the chat-page attach flow had, fixed in `useAddEntityParticipant`).
 *  - `entity_meta.project_id` must equal the conversation's own project —
 *    the resolver compares them — which is why `EditApplicationActions`
 *    renders this only for a writer: a PUBLIC agent viewed from the public
 *    project would attach cleanly and then refuse every message.
 */
/** Everything one click needs, gathered so the callback closes over ONE ref-stable object instead of eleven values (the hook-deps budget is 8). */
interface StartChatInput {
  readonly projectId: string;
  readonly applicationId: string;
  readonly name: string | undefined;
  readonly versionId: string;
  readonly agentType: string;
  readonly userId: string;
}

/**
 * `shared/api/unwrap.ts` is the ONE place envelope knowledge lives (R-A6,
 * `elitea/no-adhoc-envelope-unwrap`), so the transport peel comes from
 * `unwrapBody` rather than from a local `.data` read of this query's own
 * shape. `unwrapBody` makes no claim about the body — the cast below is the
 * caller's own, and the 401 variant it ignores is unreachable anyway
 * (`eliteaFetch` throws on non-2xx).
 */
function currentAuthorOf(data: unknown): SocialAuthorProfile | undefined {
  return unwrapBody(data) as SocialAuthorProfile | undefined;
}

export function ChatWithAgentButton({ projectId, applicationId, name, activeVersion, onError }: ChatWithAgentButtonProps): ReactNode {
  const navigate = useNavigate();
  const { mutateAsync: createConversation } = conversationApi.useCreate();
  const { mutateAsync: addParticipants } = useAddParticipantMutation();
  const [isStarting, setIsStarting] = useState(false);
  // The signed-in user, because the USER participant is the client's to add:
  // nothing server-side creates one on the REST path (the socket-era
  // `findOrCreateUserParticipant` lives on the predict route this app does
  // not use), and the agent resolver joins the author through it — a
  // conversation without the mapping refuses every send as 422. The adhoc
  // send path posts the same entry (`useChatBoxSend.helpers.ts`'s
  // `adhocParticipants`).
  const authorQuery = useGetCurrentAuthor();
  const userId = currentAuthorOf(authorQuery.data)?.id;

  const versionId = activeVersion?.id;
  const agentType = activeVersion?.agent_type ?? 'openai';
  // Memoized so the callback's dependency is referentially stable — the same
  // reason the pipeline twin memoizes its copy (exhaustive-deps errors on an
  // inline object).
  const input: StartChatInput | undefined = useMemo(
    () =>
      projectId !== undefined && applicationId !== undefined && versionId !== undefined && userId !== undefined
        ? { projectId, applicationId, name, versionId, agentType, userId: String(userId) }
        : undefined,
    [projectId, applicationId, name, versionId, agentType, userId],
  );

  const handleClick = useCallback(async () => {
    if (input === undefined || isStarting) return;
    setIsStarting(true);
    let conversationId: string;
    try {
      const conversation = await createConversation({
        projectId: input.projectId,
        name: (input.name ?? '').slice(0, MAX_CONVERSATION_NAME) || t('pages.agents.chatWithAgent.defaultName', 'New Chat'),
        is_private: true,
      });
      conversationId = String(conversation.id);
      await addParticipants({
        projectId: input.projectId,
        conversationId,
        participants: [
          { entity_name: 'user', entity_meta: { id: Number(input.userId) } },
          {
            entity_name: 'application',
            entity_meta: { id: input.applicationId, ...(input.name !== undefined ? { name: input.name } : {}), project_id: input.projectId },
            entity_settings: {
              version_id: input.versionId,
              agent_type: input.agentType,
              variables: [],
              icon_meta: {},
            },
          },
        ],
      });
    } catch (error) {
      onError(error);
      setIsStarting(false);
      return;
    }
    // The chat page's participant selection makes the freshly attached
    // agent the ACTIVE participant on load, so the first send addresses it
    // — verified in a browser against the standalone stack.
    //
    // NOT awaited, and `isStarting` is cleared BEFORE this call rather than
    // from its resolution (audit, handoff brief — same finding as the
    // pipelines twin, `ChatWithPipelineButton.tsx`): this page arms
    // `useUnsavedChangesNavBlocker` while the form is dirty, and this
    // navigation is exactly the kind of nav-away that guard exists to catch
    // — so it deliberately does NOT bypass the dialog the way
    // `handleDiscarded`/`handleDeleted` do. But TanStack Router's
    // `navigate()` promise resolves only once the history push actually
    // commits (`@tanstack/router-core`'s `commitLocation`); a BLOCKED
    // attempt that the user then CANCELS never commits —
    // `@tanstack/history`'s `tryNavigation` returns as soon as the blocker
    // resolves `true` without ever calling `task()`/`notify()`, so nothing
    // ever resolves that promise. Measured against the unfixed button (this
    // page's twin's own test, `EditPipeline.test.tsx`'s "recovers the
    // button instead of hanging…", reproduced here too): `await
    // navigate(...)` left `isStarting` stuck `true` — "Opening chat…"
    // forever — the instant the user clicked Cancel on the guard's own
    // dialog. Firing it and moving on avoids the hang while still letting
    // the dialog show and block for real; a genuine navigate() failure
    // (effectively unreachable for this static, well-formed `to`) still
    // reaches the same error surface via `.catch`.
    setIsStarting(false);
    void navigate({ to: '/chat/$conversationId', params: { conversationId } }).catch(onError);
  }, [input, isStarting, createConversation, addParticipants, navigate, onError]);

  return (
    <Button
      variant="outlined"
      color="secondary"
      size="small"
      data-testid="chat-with-agent-button"
      disabled={input === undefined || isStarting}
      onClick={() => void handleClick()}
    >
      {isStarting
        ? t('pages.agents.chatWithAgent.starting', 'Opening chat…')
        : t('pages.agents.chatWithAgent.label', 'Chat')}
    </Button>
  );
}
