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

export interface ChatWithPipelineButtonProps {
  readonly projectId: string | undefined;
  /** The pipeline's id, as the route carries it. */
  readonly applicationId: string | undefined;
  readonly name: string | undefined;
  /** The version the chat runs — the participant pins it, see the mapping note below. */
  readonly activeVersion: ApplicationVersionDetail | undefined;
  /** Called with the failure, so the caller can show the server's own refusal (e.g. FU-244 composition). */
  readonly onError: (error?: unknown) => void;
}

/**
 * "Chat" — the way to actually TALK to the pipeline being edited. The
 * pipelines twin of `pages/agents/ui/ChatWithAgentButton.tsx` (an
 * independent, same-body local copy — `no-sideways-features`/the layer model
 * forbids importing across a `pages/agents` <-> `pages/pipelines` boundary,
 * the same posture `../useDiscardPipelineChanges.ts` documents in full).
 * Before it, the edit page's own chat pane was a disclosed gap
 * (`../lib/pipelineConfigurationTabGaps.tsx`'s chat slot) and nothing else
 * offered a way in: a pipeline could be authored but never spoken to.
 *
 * One click creates a conversation, attaches the SIGNED-IN USER and the
 * pipeline as participants, and lands in the real chat surface — the same
 * three steps the chat page's "+" menu performs, addressed from the
 * pipeline's side instead.
 *
 * THE USER PARTICIPANT IS NOT OPTIONAL, and it is the client's to add:
 * nothing server-side creates the mapping on the REST path (the socket-era
 * `findOrCreateUserParticipant` lives on the predict route this app does not
 * use), and pipelines resolve through the SAME query agents do
 * (`services/elitea-main/internal/db/queries/agent_chat.sql`), whose author
 * join is an INNER JOIN on a `user` participant with
 * `entity_meta->>'id'` equal to the acting user. A conversation without it
 * answers 422 on every send — the pipeline attaches cleanly and then refuses
 * to talk. Hence the button also waits for the author read before enabling:
 * a click that cannot name the user would create exactly that dead
 * conversation. The adhoc send path posts the same entry
 * (`useChatBoxSend.helpers.ts`'s `adhocParticipants`).
 *
 * THE ONE MAPPING DIFFERENCE from the agents twin: a pipeline participant is
 * `entity_name: 'application'` with `entity_settings.agent_type: 'pipeline'`
 * — the resolver treats pipelines as applications behind the `agent_type`
 * discriminator (`features/chat-participants/lib/helpers.ts`'s
 * `buildNonModelParticipant`: `participant.agent_type === 'pipeline'` maps to
 * `ChatParticipantType.Applications`). `entity_settings.version_id` is
 * LOAD-BEARING exactly as for agents — the version join reads that key, and a
 * participant without it answers 422 on every turn. `entity_meta.project_id`
 * must equal the conversation's own project, which is why the page renders
 * this only for a writer (same `isReadOnlyView` gate as the save bar).
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
 * The transport peel comes from `shared/api/unwrap.ts` — the ONE place
 * envelope knowledge lives (R-A6, `elitea/no-adhoc-envelope-unwrap`) — not
 * from a hand-rolled `.data` read, and not from the agents twin, which the
 * layer model puts out of reach anyway (no `pages/pipelines` -> `pages/agents`
 * edge, see this file's header). `unwrapBody` makes no claim about the body,
 * so the cast below is the caller's own; the 401 variant it ignores is
 * unreachable (`eliteaFetch` throws on non-2xx).
 */
function currentAuthorOf(data: unknown): SocialAuthorProfile | undefined {
  return unwrapBody(data) as SocialAuthorProfile | undefined;
}

export function ChatWithPipelineButton({ projectId, applicationId, name, activeVersion, onError }: ChatWithPipelineButtonProps): ReactNode {
  const navigate = useNavigate();
  const { mutateAsync: createConversation } = conversationApi.useCreate();
  const { mutateAsync: addParticipants } = useAddParticipantMutation();
  const [isStarting, setIsStarting] = useState(false);
  // The signed-in user, because the USER participant is the client's to add —
  // see the author-join note above.
  const authorQuery = useGetCurrentAuthor();
  const userId = currentAuthorOf(authorQuery.data)?.id;

  const versionId = activeVersion?.id;
  const agentType = activeVersion?.agent_type;
  // Memoized (a deviation from the agents twin, which builds this inline):
  // oxlint's exhaustive-deps flags an unmemoized object dependency of the
  // click callback as changing every render.
  const input = useMemo<StartChatInput | undefined>(
    () =>
      projectId !== undefined && applicationId !== undefined && versionId !== undefined && userId !== undefined
        ? { projectId, applicationId, name, versionId, agentType: agentType ?? 'pipeline', userId: String(userId) }
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
        name: (input.name ?? '').slice(0, MAX_CONVERSATION_NAME) || t('pages.pipelines.chatWithPipeline.defaultName', 'New Chat'),
        is_private: true,
      });
      conversationId = String(conversation.id);
      await addParticipants({
        projectId: input.projectId,
        conversationId,
        participants: [
          // FIRST, and as a NUMBER: the resolver's author join compares
          // `entity_meta->>'id'` to the acting user id.
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
    // pipeline the ACTIVE participant on load, so the first send addresses
    // it — same behaviour the agents twin verified in a browser against the
    // standalone stack.
    //
    // NOT awaited, and `isStarting` is cleared BEFORE this call rather than
    // from its resolution (audit, handoff brief): this page arms
    // `useUnsavedChangesNavBlocker` while the form is dirty, and this
    // navigation is exactly the kind of nav-away that guard is meant to
    // catch — so it deliberately does NOT bypass the dialog the way
    // `handleDiscarded` does. But TanStack Router's `navigate()` promise
    // resolves only once the history push actually commits
    // (`@tanstack/router-core`'s `commitLocation`); a BLOCKED attempt that
    // the user then CANCELS never commits — `@tanstack/history`'s
    // `tryNavigation` returns as soon as the blocker resolves `true`
    // without ever calling `task()`/`notify()`, so nothing ever resolves
    // that promise. Measured against the unfixed button (this file's own
    // test, "recovers the button instead of hanging…"): `await
    // navigate(...)` here left `isStarting` stuck `true` — "Opening chat…"
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
      data-testid="chat-with-pipeline-button"
      disabled={input === undefined || isStarting}
      onClick={() => void handleClick()}
    >
      {isStarting
        ? t('pages.pipelines.chatWithPipeline.starting', 'Opening chat…')
        : t('pages.pipelines.chatWithPipeline.label', 'Chat')}
    </Button>
  );
}
