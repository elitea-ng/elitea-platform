/**
 * stale-disclosure-core.mjs — the rules behind `check-stale-disclosure.mjs`
 * (issue 621).
 *
 * WHAT GOES WRONG WITHOUT THIS GATE
 *
 * A disclosed gap is a claim about the corpus AT A MOMENT IN TIME. "NOT
 * PORTED", "NO PRODUCER", "never emitted today" were all true when they were
 * written. Nothing re-checks one when the corpus changes, so the comment stays
 * and the code moves out from under it. Auditing the port inventory in issue
 * 615 disproved THIRTEEN such rows, and almost every one was a `NOT PORTED`
 * comment that a later reader had transcribed as evidence.
 *
 * The costliest one shipped inside a generated client:
 * internal/api/generated/api.gen.go carried "centry.audit_events — the table
 * that had one — is READ-ONLY from this service. Never emitted today." long
 * after issue 615 gave that table a writer. Two files in this repository
 * (internal/api/v2/mcp/registry.go, internal/infra/db/repos/analytics.go) say
 * about THEMSELVES that their claim is true only at a moment in time and that
 * nothing re-checks it. This module is the thing that re-checks it.
 *
 * THE MECHANISM
 *
 * A claim in a scoped file must carry a FALSIFIER beside it — a line of the
 * form
 *
 *     DISCLOSURE-CHECK: absent  `<literal>` in <repo-relative path>
 *     DISCLOSURE-CHECK: present `<literal>` in <repo-relative path>
 *
 * `absent` says the claim holds only while that literal is NOWHERE under that
 * path. `present` says the claim's stated evidence must still be there. The
 * gate runs every probe and fails when a verdict flips. Choose a literal that
 * the CLOSING of the gap would create or destroy — the signature of a stub
 * handler, the assignment of the key that has no producer — so the probe fires
 * on the day the claim stops being true, and not before.
 *
 * FOUR WAYS THIS GATE COULD LIE, AND WHAT STOPS EACH
 *
 *   1. A claim with no probe would pass by having nothing to check. It is
 *      reported as `unverified`.
 *   2. A probe left behind after its claim was deleted would keep passing and
 *      prove nothing. It is reported as `orphan`.
 *   3. A probe whose path matches NO file finds nothing, and "found nothing"
 *      is the same output as "the claim holds". It is reported as `blind` —
 *      this is the "absence reads as correctness" trap the gate exists to
 *      close, and it would be absurd to reproduce it here.
 *   4. A malformed probe line would be skipped silently. It is reported as
 *      `malformed`.
 *
 * A FIFTH: a probe quotes its own literal, so a naive search finds the probe
 * itself and every `absent` probe reports the gap closed. `literalOccurs`
 * ignores any line that carries the tag. A probe is therefore a SINGLE-LINE
 * literal — a literal split over two lines cannot be found, by design, because
 * the alternative is a search that reads its own annotation as evidence.
 *
 * Everything here is pure: text in, verdicts out. The caller owns the file
 * system, the printing and the exit code.
 */

/**
 * The claim markers. Each one is a phrase this repository uses to disclose a
 * gap, not ordinary prose.
 *
 * `NOT PORTED` and `NO PRODUCER` are matched in UPPER CASE only: that is how
 * this repository writes them as a claim, and the lower-case words appear in
 * ordinary sentences that assert nothing ("a figure with no producer is
 * ABSENT rather than present and zero"). `no producer` in a Go doc comment is
 * the one lower-case form that IS used as a claim, so it is matched with a
 * dash or an em dash in front of it, which is how those comments write it.
 *
 * @type {ReadonlyArray<{id: string, pattern: RegExp}>}
 */
export const CLAIM_MARKERS = Object.freeze([
  { id: 'not-ported', pattern: /\bNOT PORTED\b/ },
  { id: 'no-producer', pattern: /\bNO PRODUCER\b|[-—]\s*no producer\b/ },
  { id: 'never-emitted', pattern: /never emitted/i },
]);

/** The probe verbs, and what each asserts about the literal. */
export const PROBE_VERBS = Object.freeze(['absent', 'present']);

/**
 * How far below a probe a claim may sit and still be covered by it.
 *
 * Probes attach FORWARD only, because that is where this repository puts them:
 * above the property, the field or the doc sentence they justify. The window is
 * one description block of `services/elitea-main/api/openapi/v2.yaml`, whose
 * longest annotated block runs to 13 lines. A window that is too WIDE only
 * over-verifies — a neighbouring claim gets checked by a probe that was not
 * written for it — while a window that is too NARROW leaves a claim
 * unverified, which is a violation. So the number errs wide.
 */
export const DEFAULT_WINDOW = 15;

const PROBE_TAG = 'DISCLOSURE-CHECK:';
const PROBE_RE = /DISCLOSURE-CHECK:\s*(\w+)\s+`([^`]+)`\s+in\s+(\S+)\s*$/;

/**
 * Report whether `literal` occurs in `text`, ignoring every probe line.
 *
 * A probe quotes the literal it looks for. Without this rule an `absent` probe
 * finds its own annotation and reports the gap closed on the day it is
 * written — the gate would fail every claim it was asked to protect.
 *
 * @param {string} text the corpus file to search.
 * @param {string} literal the substring to look for, on one line.
 * @returns {boolean} whether any non-probe line contains it.
 */
export function literalOccurs(text, literal) {
  return text.split('\n').some((line) => !line.includes(PROBE_TAG) && line.includes(literal));
}

/**
 * @typedef {object} Claim
 * @property {number} line 1-based line number.
 * @property {string} marker which CLAIM_MARKERS entry matched.
 * @property {string} text the whole line, trimmed.
 */

/**
 * Find every disclosure claim in one file's text.
 *
 * A line that carries a probe is never itself a claim: a probe quotes the
 * literal it looks for, and that literal may contain the marker words.
 *
 * @param {string} text the file's contents.
 * @returns {Claim[]} every claim, in file order. One line yields at most one
 *   claim, named by the FIRST marker that matches it.
 */
export function findClaims(text) {
  const claims = [];
  const lines = text.split('\n');
  for (let i = 0; i < lines.length; i += 1) {
    const line = lines[i];
    if (line.includes(PROBE_TAG)) continue;
    const marker = CLAIM_MARKERS.find((candidate) => candidate.pattern.test(line));
    if (marker) claims.push({ line: i + 1, marker: marker.id, text: line.trim() });
  }
  return claims;
}

/**
 * @typedef {object} Probe
 * @property {number} line 1-based line number.
 * @property {string} raw the whole line, trimmed.
 * @property {string} [verb] one of PROBE_VERBS.
 * @property {string} [literal] the substring to look for.
 * @property {string} [target] the repo-relative file or directory to look in.
 * @property {string|null} error why the line does not parse, or null.
 */

/**
 * Find every probe line in one file's text, parsed or not.
 *
 * A line that carries the tag and does not parse is returned WITH its error
 * rather than dropped. A probe nobody can read is a probe nobody runs, and
 * dropping it would let a typo silence a claim.
 *
 * @param {string} text the file's contents.
 * @returns {Probe[]} every probe line, in file order.
 */
export function findProbes(text) {
  const probes = [];
  const lines = text.split('\n');
  for (let i = 0; i < lines.length; i += 1) {
    const line = lines[i];
    if (!line.includes(PROBE_TAG)) continue;
    const raw = line.trim();
    const match = PROBE_RE.exec(line);
    if (!match) {
      probes.push({
        line: i + 1,
        raw,
        error: 'does not parse; write: DISCLOSURE-CHECK: <absent|present> `<literal>` in <path>',
      });
      continue;
    }
    const [, verb, literal, target] = match;
    if (!PROBE_VERBS.includes(verb)) {
      probes.push({ line: i + 1, raw, error: `unknown verb ${JSON.stringify(verb)}; use absent or present` });
      continue;
    }
    probes.push({ line: i + 1, raw, verb, literal, target, error: null });
  }
  return probes;
}

/**
 * Decide which probes cover which claims.
 *
 * @param {readonly Claim[]} claims every claim of one file.
 * @param {readonly Probe[]} probes every probe of the same file.
 * @param {number} [window] how far a probe reaches below itself.
 * @returns {{pairs: Array<{claim: Claim, probes: Probe[]}>, orphans: Probe[]}}
 *   `pairs` carries one entry per claim; a claim with no probe gets an empty
 *   array, which the caller reports. `orphans` are probes that cover nothing.
 */
export function attachProbes(claims, probes, window = DEFAULT_WINDOW) {
  const covered = new Set();
  const pairs = claims.map((claim) => {
    const own = probes.filter(
      (probe) => probe.error === null && probe.line <= claim.line && claim.line - probe.line <= window,
    );
    for (const probe of own) covered.add(probe.line);
    return { claim, probes: own };
  });
  const orphans = probes.filter((probe) => probe.error === null && !covered.has(probe.line));
  return { pairs, orphans };
}

/**
 * @typedef {object} ProbeResult
 * @property {boolean} matched whether the literal was found under the target.
 * @property {number} filesSearched how many files the target resolved to.
 */

/**
 * @typedef {object} Violation
 * @property {string} file the scoped file, as the caller named it.
 * @property {number} line 1-based line of the claim or the probe.
 * @property {'unverified'|'malformed'|'orphan'|'blind'|'stale'} kind
 * @property {string} message what to do about it.
 */

/**
 * Judge one file.
 *
 * @param {object} input
 * @param {string} input.file the file's name, for the messages.
 * @param {string} input.text its contents.
 * @param {(probe: Probe) => ProbeResult} input.runProbe searches the corpus.
 *   Injected so this module never touches a file system.
 * @param {number} [input.window] how far a probe reaches below itself.
 * @returns {{violations: Violation[], claims: number, probes: number}} the
 *   counts are for the caller's floor check: a scoped file that yields no
 *   claim at all is a scope entry nobody is maintaining.
 */
export function evaluateFile({ file, text, runProbe, window = DEFAULT_WINDOW }) {
  const claims = findClaims(text);
  const probes = findProbes(text);
  const violations = [];

  for (const probe of probes) {
    if (probe.error !== null) {
      violations.push({ file, line: probe.line, kind: 'malformed', message: `${probe.raw} — ${probe.error}` });
    }
  }

  const { pairs, orphans } = attachProbes(claims, probes, window);

  for (const orphan of orphans) {
    violations.push({
      file,
      line: orphan.line,
      kind: 'orphan',
      message: `no disclosure claim within ${window} lines below this probe. Delete the probe, or move it beside the claim it justifies.`,
    });
  }

  const ran = new Map();
  for (const { claim, probes: own } of pairs) {
    if (own.length === 0) {
      violations.push({
        file,
        line: claim.line,
        kind: 'unverified',
        message: `disclosure claim (${claim.marker}) with no DISCLOSURE-CHECK probe above it: ${claim.text}`,
      });
      continue;
    }
    for (const probe of own) {
      if (!ran.has(probe.line)) ran.set(probe.line, runProbe(probe));
      const result = ran.get(probe.line);
      if (result.filesSearched === 0) {
        violations.push({
          file,
          line: probe.line,
          kind: 'blind',
          message: `${probe.target} matches no file, so this probe can only ever report "not found". Point it at a path that exists.`,
        });
        continue;
      }
      if (probe.verb === 'absent' && result.matched) {
        violations.push({
          file,
          line: claim.line,
          kind: 'stale',
          message: `the claim on line ${claim.line} says the gap is open, but \`${probe.literal}\` now exists under ${probe.target}. Re-read the claim and correct it.`,
        });
      }
      if (probe.verb === 'present' && !result.matched) {
        violations.push({
          file,
          line: claim.line,
          kind: 'stale',
          message: `the claim on line ${claim.line} rests on \`${probe.literal}\` under ${probe.target}, and it is not there any more. Re-read the claim and correct it.`,
        });
      }
    }
  }

  return { violations, claims: claims.length, probes: probes.length };
}
