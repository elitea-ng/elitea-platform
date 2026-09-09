/**
 * The webhook event catalogue (#876's second half) — a picker's option list
 * mirroring the Go source of truth, `internal/events.Catalogue`
 * (services/elitea-main/internal/events/publisher.go).
 *
 * This is a MIRROR, not a fetch: the `webhooks.events` field stays free text
 * on the wire (WebhookWriteRequest's OpenAPI description says so explicitly
 * — "the server enforces no fixed vocabulary"), so a picker that only ever
 * offered THESE options would be a regression for a project that has, or
 * wants, a custom string. `WebhookFormDialog`'s Autocomplete is `freeSolo`
 * for exactly that reason: pick from here, or type anything.
 *
 * Keep this list in sync with internal/events.Catalogue by hand — there is
 * no generated bridge from a Go `const` block to a TypeScript array, and
 * adding one for nine strings is not a change this issue also makes.
 * webhookEventCatalogue.test.ts pins the list so a drift is at least visible
 * in a diff.
 */

export interface WebhookEventCatalogueEntry {
  readonly type: string;
  readonly description: string;
  /**
   * False for an event this platform has named but no producer emits.
   * Every entry below is wired as of the SSRF-hardening follow-up to #876
   * (pipeline.run.succeeded/failed were the last two) — the field stays so
   * a future declared-ahead-of-its-producer event has somewhere to say so.
   */
  readonly wired: boolean;
}

export const WEBHOOK_EVENT_CATALOGUE: readonly WebhookEventCatalogueEntry[] = [
  { type: 'pipeline.run.started', description: 'An unattended pipeline run was admitted (inbound trigger or schedule).', wired: true },
  { type: 'pipeline.run.succeeded', description: 'An unattended pipeline run finished successfully.', wired: true },
  { type: 'pipeline.run.failed', description: 'An unattended pipeline run finished with an error, or was cancelled.', wired: true },
  { type: 'schedule.fired', description: "A pipeline's cron schedule fired and admitted a run.", wired: true },
  { type: 'agent.version.published', description: 'An agent (or pipeline) version was published to the catalog.', wired: true },
  { type: 'agent.version.unpublished', description: 'A published agent version was withdrawn.', wired: true },
  { type: 'conversation.created', description: 'A new chat conversation was created.', wired: true },
  { type: 'artifact.uploaded', description: 'A file was uploaded to a project artifact bucket.', wired: true },
  { type: 'moderation.request.decided', description: 'A moderation or project-creation request was approved or rejected.', wired: true },
];

/** {@link WEBHOOK_EVENT_CATALOGUE}'s `type` column, for the Autocomplete's `options`. */
export const WEBHOOK_EVENT_TYPES: readonly string[] = WEBHOOK_EVENT_CATALOGUE.map((entry) => entry.type);
