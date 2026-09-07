/**
 * `default_internal_mcp_enabled` — the flag behind Settings › General's
 * "Agent & Pipeline Builder" switch.
 *
 * WHERE IT LIVES. Nowhere project-scoped. There is no elitea-main route that
 * reads or writes a project-level `default_internal_mcp_enabled`; it is not
 * in `v2.yaml`, not in `libs/proto`, not in `gen/`. The reference persists it
 * the same way it persists every other Settings form field: through
 * `SettingsFormProvider`, whose `onSubmit` calls the author-description
 * mutation — `PUT /social/author` — with the flag inside the
 * `personalization` blob (`settings/ui/shared/SettingsFormProvider.jsx`,
 * `ProfileHelpers.deserializeProfileFormData`). So the switch is per-USER,
 * not per-project, in production too. This module reproduces that rather
 * than inventing a project-scoped endpoint that does not exist.
 *
 * WHY THE WHOLE BLOB IS REBUILT ON EVERY WRITE. The PUT REPLACES
 * `personalization` wholesale (the upsert's `personalization =
 * EXCLUDED.personalization`), so a body that carries only this one key
 * deletes every other setting the user has — their persona, their default
 * instructions, everything the AI Personality page owns.
 * `buildInternalMcpUpdate` therefore spreads the fetched blob under the one
 * changed key. This is the same hazard `ui/ai-personality/settingsProfileForm.ts`
 * documents at length for its own writer; the two must stay consistent, and
 * they are tested against the same shape.
 */

export interface AuthorWithPersonalization {
  readonly name?: string;
  readonly description?: string;
  readonly avatar?: string;
  readonly personalization?: unknown;
}

function readPersonalization(author: AuthorWithPersonalization | undefined): Record<string, unknown> {
  const raw = author?.personalization;
  return typeof raw === 'object' && raw !== null && !Array.isArray(raw)
    ? (raw as Record<string, unknown>)
    : {};
}

/**
 * The switch's position.
 *
 * DEFAULTS TO TRUE when the key is absent, because that is what the live
 * page shows: a user who has never touched this setting sees the switch ON,
 * and the feature is on for them. Defaulting to `false` would have drawn an
 * OFF switch over an enabled feature — a control that lies about the state
 * it reports, and one that a user "turning on" would leave exactly as it was
 * while writing a redundant record.
 */
export function selectInternalMcpEnabled(author: AuthorWithPersonalization | undefined): boolean {
  const value = readPersonalization(author)['default_internal_mcp_enabled'];
  return typeof value === 'boolean' ? value : true;
}

/**
 * The full `PUT /social/author` body for a change to this one flag: every
 * other `personalization` key carried forward, plus the top-level fields the
 * same endpoint would otherwise clear.
 */
export function buildInternalMcpUpdate(
  author: AuthorWithPersonalization | undefined,
  enabled: boolean,
): {
  name?: string;
  description?: string;
  avatar?: string;
  personalization: Record<string, unknown>;
} {
  return {
    ...(author?.name === undefined ? {} : { name: author.name }),
    ...(author?.description === undefined ? {} : { description: author.description }),
    ...(author?.avatar === undefined ? {} : { avatar: author.avatar }),
    personalization: {
      ...readPersonalization(author),
      default_internal_mcp_enabled: enabled,
    },
  };
}
