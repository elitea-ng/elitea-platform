/**
 * Does the model journal a spec just read actually belong to the stack it is
 * driving?
 *
 * ── THE FAILURE THIS EXISTS TO END ───────────────────────────────────────
 *
 * `MOCK_HOST` in `e2e/fixtures/api.ts` is
 * `http://localhost:${STANDALONE_MOCK_PORT ?? 8090}`, read from the
 * PLAYWRIGHT process's environment — not from the stack the run is pointed
 * at. A second standalone stack publishes its mock on a different port, so a
 * run against `:8088` whose invocation forgot `STANDALONE_MOCK_PORT` clears
 * and reads the OTHER stack's journal. Nothing errors: the reads succeed, the
 * journal is simply somebody else's.
 *
 * What the spec then reports is an accusation against the product. Measured,
 * on three journeys at once: "the conversation toolkit was never injected into
 * the ad-hoc turn", "the agent participant was never offered to the bare-model
 * turn as a callable tool", and a 300k attachment that looked like it reached
 * the model. All three were green against the right port. A whole bisect was
 * spent on the wrong question before the port was noticed.
 *
 * That is this repository's recurring "absence reads as correctness" shape,
 * with an extra turn of the screw: the absence is not even the product's.
 *
 * ── WHY THE CREDENTIAL IS THE DISCRIMINATOR ──────────────────────────────
 *
 * Every model request the mock serves is journalled with a label for the
 * credential that reached it (`_credential_label`, `deploy/mock-llm/server.py`),
 * and a seeded key is recorded verbatim because it is public: `mock-key-project-<id>`.
 * So an entry NAMES the project whose catalogue credential resolved. A journal
 * holding no entry for the project under test is either the wrong mock or a
 * window in which this project made no model request at all — and for a caller
 * that is about to assert on its own traffic, both are the same mistake.
 *
 * Pure and dependency-free so it can be unit-tested without a browser, a
 * stack, or Playwright; `readMockLlmJournal` is the one caller.
 */

/** The prefix `deploy/mock-llm/server.py` records a seeded key under. */
const MOCK_CREDENTIAL_PREFIX = 'mock-key-';

/**
 * The credential label a seeded project key is journalled as.
 *
 * @param {string|number} projectId
 * @returns {string}
 */
export function mockProjectCredential(projectId) {
  return `${MOCK_CREDENTIAL_PREFIX}project-${String(projectId)}`;
}

/**
 * The message to fail with when the journal cannot be the one under test, or
 * `undefined` when it can.
 *
 * ADDITIVE BY CONSTRUCTION: a caller that names no project gets no check, so
 * every journal reader that predates this one behaves exactly as it did. The
 * check is an opt-in a spec makes when it is about to assert on traffic it
 * believes it produced.
 *
 * @param {{
 *   entries: readonly { credential?: unknown }[],
 *   projectId?: string | number | null,
 *   host: string,
 * }} input
 * @returns {string | undefined}
 */
export function mockLlmJournalScopeFailure({ entries, projectId, host }) {
  if (projectId === undefined || projectId === null || String(projectId) === '') {
    return undefined;
  }
  const wanted = mockProjectCredential(projectId);
  if (entries.some((entry) => entry.credential === wanted)) {
    return undefined;
  }
  // What the journal DOES hold, named rather than counted: "2 entries" says
  // nothing, whereas `mock-key-project-1` beside a run against project 90107
  // is the whole diagnosis in one line.
  const seen = [...new Set(entries.map((entry) => String(entry.credential ?? 'none')))]
    .slice(0, 5)
    .join(', ');
  return (
    `the model journal at ${host} holds no request for project ${String(projectId)} ` +
    `(expected an entry with credential "${wanted}"; it holds: ${seen || '<empty>'}). ` +
    'Either this run produced no model request for that project, or — far more often — ' +
    'STANDALONE_MOCK_PORT is unset or wrong and this read went to ANOTHER stack\'s mock: ' +
    `MOCK_HOST defaults to :8090, so a run against a stack whose mock is published elsewhere ` +
    'must pass STANDALONE_MOCK_PORT to the Playwright process, not only to the stack script.'
  );
}
