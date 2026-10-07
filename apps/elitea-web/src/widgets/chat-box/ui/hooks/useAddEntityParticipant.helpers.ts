/**
 * Selection logic for the composer's "+" menu, split out of
 * `useAddEntityParticipant.ts`.
 *
 * Its own module for two reasons. The hook's `onSelectParticipant` callback
 * is capped at 8 dependency-array entries (§3.5 hook-deps) and the selection
 * now needs nine things (project, conversation, participant list, both
 * mutations, the detail fetch, the create-conversation adapter and both
 * notifications), so they travel in one runtime object read through a ref
 * instead of being closed over. And the async part is worth testing without a
 * React tree: everything below is a plain function over injected callbacks.
 */
import type { Participant } from '@/entities/participant';
import { ChatParticipantType, transformParticipant } from '@/features/chat-participants';

type CatalogSelection = Readonly<Record<string, unknown>>;

/** The POST body one "+"-menu selection becomes. */
export interface ParticipantInput {
  readonly entity_name: string;
  readonly entity_meta: Record<string, unknown>;
  readonly entity_settings: Record<string, unknown>;
}

export function selectedParticipantInput(selection: unknown): ParticipantInput | undefined {
  const candidate = selection as CatalogSelection;
  const participantType = candidate['participantType'];
  if (
    participantType !== ChatParticipantType.Applications &&
    participantType !== ChatParticipantType.Pipelines &&
    participantType !== ChatParticipantType.Toolkits
  ) return undefined;
  const transformed = transformParticipant(participantType, candidate);
  return {
    entity_name: transformed.entity_name,
    entity_meta: { ...transformed.entity_meta },
    entity_settings: { ...transformed.entity_settings },
  };
}

function selectionType(selection: CatalogSelection): string {
  const type = selection['participantType'];
  return typeof type === 'string' ? type : '';
}

/** Ids are strings or numbers on this wire ("Numeric id serialized as string", `shared/api/generated/model/applicationVersionDetail.zod.ts:54`); anything else is not an id and compares as absent. */
function idText(value: unknown): string {
  return typeof value === 'string' || typeof value === 'number' ? String(value) : '';
}

export function isToolkitSelection(selection: unknown): boolean {
  return selectionType(selection as CatalogSelection) === ChatParticipantType.Toolkits;
}

function participantTypeMatches(participant: Participant, selection: CatalogSelection): boolean {
  const type = selectionType(selection);
  if (type === ChatParticipantType.Pipelines) {
    return participant.entityName === ChatParticipantType.Pipelines ||
      (participant.entityName === ChatParticipantType.Applications && participant.entitySettings?.agentType === 'pipeline');
  }
  if (type === ChatParticipantType.Applications) {
    return participant.entityName === ChatParticipantType.Applications && participant.entitySettings?.agentType !== 'pipeline';
  }
  return type === ChatParticipantType.Toolkits && participant.entityName === ChatParticipantType.Toolkits;
}

export function findSelectedConversationParticipant(
  selection: unknown,
  participants: readonly Participant[],
): Participant | undefined {
  const candidate = selection as CatalogSelection;
  const transformed = selectedParticipantInput(candidate);
  if (!transformed) return undefined;
  const entityId = transformed.entity_meta['id'];
  const projectId = transformed.entity_meta['project_id'];
  return participants.find((participant) =>
    participantTypeMatches(participant, candidate) &&
    idText(participant.entityMeta?.id) === idText(entityId) &&
    idText(participant.entityMeta?.projectId) === idText(projectId),
  );
}

export function selectionKey(selection: unknown): string | undefined {
  const candidate = selection as CatalogSelection;
  const transformed = selectedParticipantInput(candidate);
  if (!transformed) return undefined;
  const entityId = transformed.entity_meta['id'];
  const projectId = transformed.entity_meta['project_id'];
  return `${selectionType(candidate)}:${idText(entityId)}:${idText(projectId)}`;
}

export function canBecomeActive(participant: Participant): boolean {
  return participant.entityName === ChatParticipantType.Applications ||
    participant.entityName === ChatParticipantType.Pipelines;
}

/** `useFetchParticipantDetails`'s `fetchOriginalDetails`, narrowed to what the selection needs. */
type FetchParticipantDetails = (
  type: ChatParticipantType,
  id: string,
  projectId: string,
) => Promise<Record<string, unknown>>;

/** A conversation the selection had to create because the chat had none yet. */
export interface CreatedConversation {
  readonly id?: string | number;
  readonly uuid?: string;
  /** The attached participant selected while the conversation is created. */
  readonly activeParticipantId?: string;
}

/**
 * Merges the agent's DETAIL response into the row the "+" menu was populated
 * from, so `transformParticipant` can read a version off it.
 *
 * The menu's agent/pipeline rows come from the LIST endpoint (`GET
 * /elitea_core/applications/prompt_lib/{projectId}?agents_type=classic`),
 * whose rows are `{id, project_id, name, description, created_at, updated_at,
 * owner_id, authors, is_forked, meta, has_interrupt, agent_type}` — no
 * version in any form. The participant was therefore created with
 * `entity_settings = {"icon_meta":{},"variables":[],"agent_type":"openai"}`,
 * and the resolver's join (`services/elitea-main/internal/db/queries/
 * agent_chat.sql:151-153`, matching `application_versions.id` against
 * `entity_settings ->> 'version_id'`) found no row, so
 * `internal/infra/db/repos/agent_start.go:118-119` mapped `pgx.ErrNoRows` to
 * `ErrUnsupportedCurrentAgentStart` and every turn addressed to a
 * just-added agent came back 422. Measured on a live stack: writing
 * `version_id` into that row by hand made the identical send succeed.
 *
 * The DETAIL endpoint (`GET /elitea_core/application/prompt_lib/{projectId}/
 * {id}`) does carry `version_details` — the version the backend itself marks
 * as current — and the app already called it for the participant's name and
 * icon, just too late to be used. `fetchDetails` is
 * `useFetchParticipantDetails`'s own `fetchOriginalDetails`, so this fetch
 * populates the very TanStack cache entry that later read serves from rather
 * than opening a second, private one.
 */
async function withResolvedVersion(
  selection: unknown,
  fetchDetails: FetchParticipantDetails,
  fallbackProjectId: string | number,
): Promise<unknown> {
  const candidate = selection as CatalogSelection;
  const type = selectionType(candidate);
  if (type !== ChatParticipantType.Applications && type !== ChatParticipantType.Pipelines) return selection;
  // Only the rows that lack a version cost a request: a selection made
  // somewhere that already carries one (an attached participant re-offered by
  // the recommendation list, say) transforms to a `version_id` already.
  if (selectedParticipantInput(candidate)?.entity_settings['version_id'] !== undefined) return selection;
  const entityId = idText(candidate['id']);
  if (entityId === '') return selection;
  const projectId = idText(candidate['project_id']) || idText(fallbackProjectId);
  const details = await fetchDetails(type, entityId, projectId);
  // Only emptiness is checked: every branch of `fetchOriginalDetails` returns an
  // object (`result?.data || {}`, or the toolkit scan's `{}`), so `!details` was dead.
  if (Object.keys(details).length === 0) return selection;
  return { ...candidate, ...details };
}

/** Everything one selection needs, read fresh off a ref so the hook's callback keeps two dependencies. */
export interface ParticipantSelectionRuntime {
  readonly projectId: string | number | undefined;
  readonly conversationId: string | number | undefined;
  readonly onChangeParticipant?: ((participant: unknown) => void) | undefined;
  readonly addParticipant: (input: {
    projectId: string | number;
    conversationId: string;
    participants: readonly ParticipantInput[];
  }) => Promise<Participant[]>;
  readonly deleteParticipant: (input: { projectId: string | number; conversationId: string; id: string }) => Promise<void>;
  readonly fetchDetails: FetchParticipantDetails;
  /** Creates the conversation a brand-new chat does not have yet — see `applyParticipantSelection`. */
  readonly createConversation?: (() => Promise<CreatedConversation | undefined>) | undefined;
  readonly onConversationCreated?: ((conversation: CreatedConversation) => void) | undefined;
}

async function conversationForSelection(
  runtime: ParticipantSelectionRuntime,
): Promise<{ readonly id: string; readonly created?: CreatedConversation } | undefined> {
  if (runtime.conversationId !== undefined) return { id: String(runtime.conversationId) };
  const created = await runtime.createConversation?.();
  if (created?.id === undefined) return undefined;
  return { id: String(created.id), created };
}

function withActiveParticipant(conversation: CreatedConversation, active: Participant | undefined): CreatedConversation {
  return active ? { ...conversation, activeParticipantId: active.id } : conversation;
}

/**
 * Attaches (or, for a toolkit already attached, detaches) one "+"-menu
 * selection.
 *
 * On a brand-new chat there is no conversation to attach to, and the hook
 * used to return on that alone: the click produced no request, no chip, no
 * error and no console line. The old app stages the pick and creates the
 * conversation on first send (`apps/elitea-ui/src/pages/NewChat/
 * NewConversationView.jsx:479` and its `onSend`), but this app's send path
 * cannot address a staged agent: `useChatBoxSend`'s `startStreamedExecution`
 * resolves the turn's target out of the participant list captured at render
 * (`useChatBoxSend.helpers.ts`'s `resolveTargetParticipant`) and
 * `buildStartBody` needs the participant ROW id, which only exists once the
 * POST has happened. A participant staged during a send would therefore be
 * invisible to that same send, and the turn would silently go to the model
 * instead of the agent. So the conversation is created here, eagerly, which
 * is the shape this app does have machinery for — and the very next send is
 * then an ordinary send into an existing conversation.
 */
export async function applyParticipantSelection(
  selection: unknown,
  existing: Participant | undefined,
  runtime: ParticipantSelectionRuntime,
): Promise<void> {
  const { projectId } = runtime;
  if (projectId === undefined) return;
  if (existing) {
    if (runtime.conversationId === undefined) return;
    await runtime.deleteParticipant({ projectId, conversationId: String(runtime.conversationId), id: existing.id });
    return;
  }
  // Resolved before the conversation is created, so a failing detail fetch
  // leaves no empty conversation behind.
  const resolved = await withResolvedVersion(selection, runtime.fetchDetails, projectId);
  const participantInput = selectedParticipantInput(resolved);
  if (!participantInput) return;
  const conversation = await conversationForSelection(runtime);
  if (conversation === undefined) return;
  const updated = await runtime.addParticipant({
    projectId,
    conversationId: conversation.id,
    participants: [participantInput],
  });
  // The host is told about the conversation only once the participant is on
  // it: that notification navigates to the conversation, and the route change
  // refetches its details. Announcing first would race the add and show a
  // conversation with no agent in it.
  const added = findSelectedConversationParticipant(selection, updated);
  const active = added && canBecomeActive(added) ? added : undefined;
  if (conversation.created) runtime.onConversationCreated?.(withActiveParticipant(conversation.created, active));
  if (active) runtime.onChangeParticipant?.(active);
}
