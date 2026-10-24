import type { RequestHandler } from 'msw';
import type { ZodType } from 'zod';

/**
 * R-M3 (spec §6.5): every MSW handler response is validated against its zod
 * schema AT REGISTRATION TIME — a handler whose fixture body drifts from the
 * Channel-A schema throws before any test runs, instead of feeding tests a
 * shape the real backend would never produce.
 *
 * Fixtures carry `recordedAt`, asserted present here so a fixture can never
 * omit it. There is no age limit: R-M4 (30-day freshness) was retired
 * 2026-10-23 because every fixture is synthetic.
 */

interface RecordedFixture {
  /** ISO-8601 date the fixture was recorded from the real Go stack (Channel B). */
  recordedAt: string;
  /** The captured response body the handler replays. */
  body: unknown;
}

export interface ValidatedHandlerEntry {
  /** Endpoint id as it appears in endpoints.manifest.json, for error context. */
  id: string;
  handler: RequestHandler;
  /** Channel-A zod schema for the response body. */
  schema: ZodType;
  /** Channel-B fixture the handler's body derives from. */
  fixture: RecordedFixture;
}

/**
 * Validates every entry and returns the plain handler list for setupServer.
 * Throws (fails the boot, R-M3) on the first entry whose fixture body does
 * not satisfy its schema, or whose fixture lacks recording metadata.
 */
export function registerValidatedHandlers(entries: readonly ValidatedHandlerEntry[]): RequestHandler[] {
  for (const entry of entries) {
    if (typeof entry.fixture.recordedAt !== 'string' || Number.isNaN(Date.parse(entry.fixture.recordedAt))) {
      throw new Error(
        `fixture-metadata: handler "${entry.id}" has a fixture without a valid recordedAt`,
      );
    }
    const result = entry.schema.safeParse(entry.fixture.body);
    if (!result.success) {
      throw new Error(
        `R-M3: handler "${entry.id}" fixture does not satisfy its zod schema:\n${result.error.message}`,
      );
    }
  }
  return entries.map((entry) => entry.handler);
}
