/**
 * What the agent and pipeline editors share when they put an entity into a
 * conversation: the identity Main's `editor_test` check accepts, and the
 * `application` participant row the resolver joins on.
 *
 * One place, because the two shapes drifting apart is exactly how a participant
 * "attaches cleanly and then answers 422 on every send": `entity_settings.version_id`
 * is what the version join reads, and `entity_meta.project_id` must equal the
 * conversation's own project.
 */

const POSITIVE_ID = /^[1-9][0-9]*$/;

/**
 * A stable key for one test subject, or `undefined` while any part is missing
 * or is not a positive decimal integer (the shape Main's `editor_test`
 * identity check accepts). Callers pass the parts in a fixed order, for
 * example user, project, application, version.
 */
export function editorTestIdentityKey(parts: readonly (string | undefined)[]): string | undefined {
  return parts.every((part) => part !== undefined && POSITIVE_ID.test(part)) ? parts.join(':') : undefined;
}

export interface ApplicationParticipantInput {
  readonly applicationId: string;
  readonly projectId: string;
  readonly name?: string | undefined;
  readonly versionId: string;
  readonly agentType: string;
}

export interface ApplicationParticipant {
  readonly entity_name: 'application';
  readonly entity_meta: Readonly<Record<string, unknown>>;
  readonly entity_settings: Readonly<Record<string, unknown>>;
}

/** The `application` participant for an agent or pipeline pinned to one version. */
export function applicationParticipant(input: ApplicationParticipantInput): ApplicationParticipant {
  return {
    entity_name: 'application',
    entity_meta: { id: input.applicationId, ...(input.name !== undefined ? { name: input.name } : {}), project_id: input.projectId },
    entity_settings: { version_id: input.versionId, agent_type: input.agentType, variables: [], icon_meta: {} },
  };
}
