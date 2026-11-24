import {
  exportApplication,
  forkAgent,
  importWizard,
  publishApplication,
  unpublishApplication,
  updateApplicationRelation,
  validateForPublish,
} from '@/shared/api/generated/applications/applications';
import type {
  ForkRequest,
  ImportWizardRequest,
  PublishRequest,
  PublishValidationResult,
} from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';

/**
 * The lifecycle plane, as the generated client already serves it.
 *
 * Every route below was generated from `v2.yaml` months ago and had ZERO
 * callers — `src/shared/api/endpoints.manifest.json` recorded `"usedBy": []`
 * for publish, unpublish, publish_validate, fork and import_wizard alike. The
 * server half is real and permission-gated; the app simply never called it.
 * Nothing here needs a spec change, and this file adds no new URL.
 *
 * What it does add is the unwrapping. `eliteaFetch` resolves the ENVELOPE
 * `{data, status, headers}` and the generated `Promise<T>` annotation is a
 * cast over it, so reading a response as the body gives undefined fields on a
 * perfectly good 200. Every function here reads through `.data`.
 */

interface Envelope<T> {
  readonly data: T;
  readonly status: number;
}

/**
 * The FAIL body of `validateForPublish`, which arrives as an error rather than
 * a response. An absent body becomes an empty result, which
 * `normalizePublishValidation` reads as a FAIL with no findings — not as a
 * pass.
 */
function body422(payload: unknown): PublishValidationResult {
  if (typeof payload !== 'object' || payload === null) return {};
  return payload;
}

function body<T>(envelope: unknown): T {
  return (envelope as Envelope<T>).data;
}

/** The export document, which is also the import and fork document. */
export interface EntityExportDocument {
  readonly ok?: boolean;
  readonly applications?: readonly unknown[];
  readonly toolkits?: readonly unknown[];
  readonly skills?: readonly unknown[];
}

export interface PublishOutcome {
  readonly publicAgentId?: string;
  readonly publicVersionId?: string;
  readonly versionName?: string;
  /**
   * The catalogue rows the publish created, present only when the author's
   * project is not itself the public project. Omitted rather than zeroed by
   * the server (`internal/api/v2/eliteacore/handler.go`), so `undefined` here
   * means "the clone already IS the catalogue row", never "row 0".
   */
  readonly catalogAgentId?: string;
  readonly catalogVersionId?: string;
}

/**
 * Runs the pre-publish validation.
 *
 * A **422 is a result, not a failure**: `validateForPublish` answers 200 on
 * PASS/WARN and 422 on FAIL with the identical body. `eliteaFetch` throws an
 * `EliteaApiError` on any non-2xx, so the FAIL body has to be recovered from
 * the thrown error — reporting it as a transport error would show the author
 * "validation could not run" for the one case where it ran and said no.
 */
export async function runPublishValidation(
  projectId: string,
  versionId: number,
  versionName: string,
  category?: string,
): Promise<PublishValidationResult> {
  try {
    const envelope = await validateForPublish(projectId, versionId, {
      version_name: versionName,
      ...(category === undefined || category === '' ? {} : { category }),
    });
    return body<PublishValidationResult>(envelope);
  } catch (cause) {
    if (cause instanceof EliteaApiError && cause.failure.kind === 'http' && cause.failure.status === 422) {
      return body422(cause.failure.body);
    }
    throw cause;
  }
}

export async function publishVersion(
  projectId: string,
  versionId: number,
  request: PublishRequest,
): Promise<PublishOutcome> {
  const envelope = await publishApplication(projectId, versionId, request);
  const raw = body<Record<string, unknown>>(envelope);
  return {
    ...(typeof raw['public_agent_id'] === 'string' ? { publicAgentId: raw['public_agent_id'] } : {}),
    ...(typeof raw['public_version_id'] === 'string' ? { publicVersionId: raw['public_version_id'] } : {}),
    ...(typeof raw['version_name'] === 'string' ? { versionName: raw['version_name'] } : {}),
    ...(typeof raw['catalog_agent_id'] === 'string' ? { catalogAgentId: raw['catalog_agent_id'] } : {}),
    ...(typeof raw['catalog_version_id'] === 'string' ? { catalogVersionId: raw['catalog_version_id'] } : {}),
  };
}

export async function unpublishVersion(projectId: string, versionId: number): Promise<void> {
  await unpublishApplication(projectId, versionId, {});
}

/**
 * Reads the entity as a FORK document.
 *
 * `?fork=true` is what makes the export usable as a fork input: it keeps only
 * the latest version and stamps `owner_id`, `original_exported` and the
 * `shared_*` origin the fork handler writes into `meta.parent_*`. The plain
 * export omits all four, so forking a plain export produces a copy that does
 * not know where it came from (`internal/api/v2/eliteacore/handler.go`).
 */
export async function fetchForkDocument(
  projectId: string,
  entityId: number,
  versionId?: number | string,
): Promise<EntityExportDocument> {
  // The server wants a number. Coerce, and omit the key for anything that is
  // not one (NaN from a bad route param) rather than sending `follow_version_ids=NaN`.
  const followId = versionId === undefined ? Number.NaN : Number(versionId);
  // `follow_version_ids` names the version the fork copies (#1993). Without it
  // the server keeps the newest, so forking the version an author had open
  // copied a different one.
  const envelope = await exportApplication(projectId, entityId, {
    fork: true,
    ...(Number.isInteger(followId) && followId > 0 ? { follow_version_ids: [followId] } : {}),
  });
  return body<EntityExportDocument>(envelope);
}

/** Reads the entity as a plain export document — the file Import round-trips. */
export async function fetchExportDocument(projectId: string, entityId: number): Promise<EntityExportDocument> {
  const envelope = await exportApplication(projectId, entityId);
  return body<EntityExportDocument>(envelope);
}

export interface ForkOutcome {
  /** 201 when everything forked, 207 when part of it did. */
  readonly status: number;
  readonly result: unknown;
  readonly errors: unknown;
}

async function forkEntity(targetProjectId: string, document: EntityExportDocument): Promise<ForkOutcome> {
  const request = {
    applications: document.applications ?? [],
    // The toolkits travel with the agent. The server copies each one into the
    // target project without its credentials and links the version to the
    // copy; a document sent without them forked an agent with an empty Tools
    // area (#2109).
    ...(document.toolkits === undefined ? {} : { toolkits: document.toolkits }),
    ...(document.skills === undefined ? {} : { skills: document.skills }),
  } as ForkRequest;
  const envelope = (await forkAgent(targetProjectId, request)) as Envelope<Record<string, unknown>>;
  return {
    status: envelope.status,
    result: envelope.data['result'],
    errors: envelope.data['errors'],
  };
}

/** How deep a chain of nested agents is followed, and so how many agents one fork can create. */
const MAX_NESTED_DEPTH = 3;

interface NestedAgentRef {
  readonly applicationId: number;
  readonly versionId?: number;
}

function positiveInteger(value: unknown): number | undefined {
  const parsed = typeof value === 'number' || typeof value === 'string' ? Number(value) : Number.NaN;
  return Number.isInteger(parsed) && parsed > 0 ? parsed : undefined;
}

/** The sub-agents a fork document names: its `type: application` toolkit entries, which carry the source agent's id pair. */
function nestedAgentRefs(document: EntityExportDocument): NestedAgentRef[] {
  const refs: NestedAgentRef[] = [];
  for (const entry of document.toolkits ?? []) {
    const toolkit = entry as { type?: unknown; settings?: { application_id?: unknown; application_version_id?: unknown } };
    if (toolkit.type !== 'application') continue;
    const applicationId = positiveInteger(toolkit.settings?.application_id);
    if (applicationId === undefined) continue;
    const versionId = positiveInteger(toolkit.settings?.application_version_id);
    refs.push(versionId === undefined ? { applicationId } : { applicationId, versionId });
  }
  return refs;
}

interface ForkedAgent {
  readonly applicationId: number | undefined;
  readonly versionId: number | undefined;
}

function channelEntries(channel: unknown, key: 'agents' | 'toolkits' | 'skills'): unknown[] {
  const entries = (channel as Record<string, unknown> | undefined)?.[key];
  return Array.isArray(entries) ? entries : [];
}

function firstForkedAgent(outcome: ForkOutcome): ForkedAgent {
  const agent = channelEntries(outcome.result, 'agents')[0] as Record<string, unknown> | undefined;
  return { applicationId: positiveInteger(agent?.['id']), versionId: positiveInteger(agent?.['version_id']) };
}

type Channels = Record<'agents' | 'toolkits' | 'skills', unknown[]>;

function emptyChannels(): Channels {
  return { agents: [], toolkits: [], skills: [] };
}

/** The three channels of one answer, as arrays. */
function channelsOf(answer: unknown): Channels {
  return { agents: channelEntries(answer, 'agents'), toolkits: channelEntries(answer, 'toolkits'), skills: channelEntries(answer, 'skills') };
}

function withChannels(answer: unknown, extra: Channels): unknown {
  const own = channelsOf(answer);
  return {
    ...((answer ?? {}) as Record<string, unknown>),
    agents: [...own.agents, ...extra.agents],
    toolkits: [...own.toolkits, ...extra.toolkits],
    skills: [...own.skills, ...extra.skills],
  };
}

function addChannels(into: Channels, from: Channels): void {
  into.agents.push(...from.agents);
  into.toolkits.push(...from.toolkits);
  into.skills.push(...from.skills);
}

interface NestedForkContext {
  readonly sourceProjectId: string;
  readonly targetProjectId: string;
  /** Source agent id to the agent created for it, so an agent reached twice is created once. */
  readonly created: Map<number, ForkedAgent>;
  readonly depth: number;
}

/** Creates one nested agent (unless it already was) and links it under `parent`. Reports what it made on `added` and what went wrong on `failed`. */
async function forkNestedRef(context: NestedForkContext, ref: NestedAgentRef, parent: { readonly applicationId: number; readonly versionId: number }, added: Channels, failed: Channels): Promise<void> {
  let child = context.created.get(ref.applicationId);
  if (child === undefined) {
    if (context.depth >= MAX_NESTED_DEPTH) throw new Error('it is nested too deeply');
    const nestedDocument = await fetchForkDocument(context.sourceProjectId, ref.applicationId, ref.versionId);
    const nested = await forkEntityWithNested(context.sourceProjectId, context.targetProjectId, nestedDocument, context.created, context.depth + 1);
    // Recorded before the link, so a child that was created but not linked is reported.
    addChannels(added, channelsOf(nested.result));
    addChannels(failed, channelsOf(nested.errors));
    child = firstForkedAgent(nested);
    context.created.set(ref.applicationId, child);
  }
  if (child.applicationId === undefined || child.versionId === undefined) throw new Error('the nested agent was not created');
  await updateApplicationRelation(context.targetProjectId, child.applicationId, child.versionId, {
    application_id: parent.applicationId,
    version_id: parent.versionId,
    has_relation: true,
  });
}

function sourceApplicationId(document: EntityExportDocument): number | undefined {
  return positiveInteger((document.applications?.[0] as { id?: unknown } | undefined)?.id);
}

/** Records the agent a fork created under the source agent's id, and returns it. */
function rememberCreated(created: Map<number, ForkedAgent>, document: EntityExportDocument, outcome: ForkOutcome): ForkedAgent {
  const agent = firstForkedAgent(outcome);
  const sourceId = sourceApplicationId(document);
  if (sourceId !== undefined && agent.applicationId !== undefined) created.set(sourceId, agent);
  return agent;
}

function hasEntries(channels: Channels): boolean {
  return channels.agents.length + channels.toolkits.length + channels.skills.length > 0;
}

/**
 * Forks an agent into another project WITH everything it nests (#2109).
 *
 * The server copies the toolkits of the document and skips each sub-agent
 * stand-in, because the sub-agent is a separate agent with its own versions.
 * So each one is read from the SOURCE project (the caller can read it there),
 * created in the target the same way, and linked to the new parent version
 * through the relation route the editor's "add agent" uses. Only credentials
 * and secrets stay behind, as the toolkit copy already drops them.
 *
 * An agent reached twice (a shared sub-agent, or one that nests its parent) is
 * created once and linked wherever it is nested. A nested agent that cannot be
 * created or linked is reported on the errors channel with status 207 and the
 * parent stays; one that was created but not linked is still in the result, so
 * nothing is created silently. A chain deeper than MAX_NESTED_DEPTH stops there.
 */
export async function forkEntityWithNested(
  sourceProjectId: string,
  targetProjectId: string,
  document: EntityExportDocument,
  created: Map<number, ForkedAgent> = new Map<number, ForkedAgent>(),
  depth = 0,
): Promise<ForkOutcome> {
  const outcome = await forkEntity(targetProjectId, document);
  const { applicationId, versionId } = rememberCreated(created, document, outcome);
  const refs = nestedAgentRefs(document);
  if (refs.length === 0 || applicationId === undefined || versionId === undefined) return outcome;

  const context: NestedForkContext = { sourceProjectId, targetProjectId, created, depth };
  const added = emptyChannels();
  const failed = emptyChannels();
  for (const ref of refs) {
    try {
      await forkNestedRef(context, ref, { applicationId, versionId }, added, failed);
    } catch (cause) {
      const reason = cause instanceof Error ? cause.message : 'unknown error';
      failed.agents.push({ index: 0, name: String(ref.applicationId), msg: `Nested agent ${String(ref.applicationId)} was not forked or linked: ${reason}` });
    }
  }
  const hasFailures = hasEntries(failed);
  return {
    status: hasFailures ? 207 : outcome.status,
    result: withChannels(outcome.result, added),
    errors: hasFailures ? withChannels(outcome.errors, failed) : outcome.errors,
  };
}

export interface ImportOutcome {
  readonly status: number;
  readonly result: unknown;
  readonly errors: unknown;
}

/**
 * Posts a whole export document to the import wizard.
 *
 * The document goes through unchanged, including its `toolkits` and `skills`
 * arrays, which the handler imports beside the applications
 * (`team_import.go` importEntitiesFrom). It used to ignore them, so every
 * imported agent with a toolkit answered 207 "unable to link tools" (issue
 * 3010). The type is asserted at this one boundary because the document's
 * entries are the export's loose records.
 */
export async function importEntities(projectId: string, document: EntityExportDocument): Promise<ImportOutcome> {
  const envelope = (await importWizard(
    projectId,
    document as unknown as ImportWizardRequest,
  )) as Envelope<Record<string, unknown>>;
  return {
    status: envelope.status,
    result: envelope.data['result'],
    errors: envelope.data['errors'],
  };
}
