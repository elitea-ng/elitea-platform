/**
 * The bulk half of "invite users": how a paste of many addresses becomes a
 * list, and how the server's per-address answer becomes something a person can
 * read.
 *
 * ## Why this is a module and not three helpers inside the dialog
 *
 * Both halves are pure string work with four named outcomes, and both are the
 * part that was missing rather than the part that was wrong. The transport has
 * accepted `{emails: [...], roles: [...]}` since the port landed, and the
 * server has answered one row per address since then too. What the product did
 * with that array was collapse it to a single toast, so an operator who pasted
 * twelve addresses and got a 400 learned only that "something" failed —
 * which of the twelve, and why, was in a response nothing rendered.
 *
 * ## The four outcomes, and why the server now names them
 *
 * pylon carried the same four states (added, already a member, malformed
 * address, write failed) only in English prose inside `msg`. Matching on prose
 * is a client that breaks when somebody rewords a log line, so elitea-main now
 * sends `outcome` beside `status`. `status` is unchanged, so this reader falls
 * back to it for a server that predates the field: an `ok` row with no
 * `outcome` is an invite, and an `error` row with no `outcome` is a failure
 * this module declines to classify further.
 */

/** One of the four states an address can end in. */
export type InviteOutcome = 'invited' | 'already_member' | 'invalid_email' | 'failed';

/** One row of the per-address array, narrowed from the untyped response. */
export interface InviteAddressResult {
  readonly email: string;
  readonly outcome: InviteOutcome;
  /** The server's own sentence. Shown as the detail line, never parsed. */
  readonly message: string;
  /** True only for an `invited` row whose invitation e-mail actually went out. */
  readonly delivered: boolean;
}

/**
 * Address separators. Comma is the reference behaviour and the one the dialog's
 * own copy promises; newline and semicolon are here because the address list an
 * operator pastes comes out of a spreadsheet column or a mail client, and
 * neither of those emits commas. Splitting on plain whitespace too is safe for
 * the same reason an address may not contain one.
 */
const SEPARATORS = /[\s,;]+/;

/**
 * Split the field into addresses, in the order they were typed, without
 * duplicates.
 *
 * De-duplication is not tidiness: the server writes one transaction per
 * address, so a list that names the same address twice makes the second copy
 * report `already_member` for a membership the FIRST copy of the same submit
 * had just created. That reads as an operator mistake and is not one.
 */
export function parseInviteAddresses(text: string): string[] {
  const seen = new Set<string>();
  const addresses: string[] = [];
  for (const candidate of text.split(SEPARATORS)) {
    const address = candidate.trim();
    if (!address) continue;
    const key = address.toLowerCase();
    if (seen.has(key)) continue;
    seen.add(key);
    addresses.push(address);
  }
  return addresses;
}

const EMAIL_RE =
  /^(([^<>()[\]\\.,;:\s@"]+(\.[^<>()[\]\\.,;:\s@"]+)*)|(".+"))@(([[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}])|(([a-zA-Z\-0-9]+\.)+[a-zA-Z]{2,}))$/;

/** Client-side address check. The server runs its own; this one is for the chips. */
export function isValidInviteAddress(address: string): boolean {
  return EMAIL_RE.test(address.trim().toLowerCase());
}

/** Every address in the field that this client already knows the server will refuse. */
export function invalidInviteAddresses(addresses: readonly string[]): string[] {
  return addresses.filter((address) => !isValidInviteAddress(address));
}

const OUTCOMES: readonly InviteOutcome[] = ['invited', 'already_member', 'invalid_email', 'failed'];

function outcomeOf(row: Record<string, unknown>): InviteOutcome {
  const declared = row.outcome;
  if (typeof declared === 'string' && (OUTCOMES as readonly string[]).includes(declared)) {
    return declared as InviteOutcome;
  }
  // Pre-`outcome` server: `status` is the only signal, and it has two values.
  return row.status === 'ok' ? 'invited' : 'failed';
}

/**
 * Narrow the response — or the body of the 400 the server answers when ANY
 * address failed — into rows.
 *
 * Both shapes are the same array. The 400 is not "the request failed": each
 * address is written in its own transaction, so the `ok` rows in a 400 body
 * have already landed, and dropping the body on the error path is how a partial
 * success came to look like a total one.
 */
export function readInviteRows(payload: unknown): InviteAddressResult[] {
  const rows = Array.isArray(payload) ? payload : undefined;
  if (!rows) return [];
  const results: InviteAddressResult[] = [];
  for (const entry of rows) {
    if (typeof entry !== 'object' || entry === null) continue;
    const row = entry as Record<string, unknown>;
    results.push({
      email: typeof row.email === 'string' ? row.email : '',
      outcome: outcomeOf(row),
      message: typeof row.msg === 'string' ? row.msg : '',
      delivered: row.invitation_delivered === true,
    });
  }
  return results;
}

/**
 * Dig the rows out of whatever the mutation rejected with.
 *
 * `eliteaFetch` throws `EliteaApiError`, whose `failure.body` is the parsed
 * response body for a `kind: 'http'` failure. Reading it here — rather than in
 * the page — keeps the one piece of knowledge about that error's shape beside
 * the reader that needs it.
 */
export function readInviteRowsFromError(error: unknown): InviteAddressResult[] {
  if (typeof error !== 'object' || error === null) return [];
  const failure = (error as { failure?: unknown }).failure;
  if (typeof failure !== 'object' || failure === null) return [];
  return readInviteRows((failure as { body?: unknown }).body);
}

export interface InviteSummary {
  readonly invited: number;
  readonly alreadyMember: number;
  readonly invalid: number;
  readonly failed: number;
  /** True when every `invited` row also sent its invitation e-mail. */
  readonly delivered: boolean;
}

/** Count the rows by outcome, for the one-line toast over the per-address list. */
export function summariseInviteRows(rows: readonly InviteAddressResult[]): InviteSummary {
  const invited = rows.filter((row) => row.outcome === 'invited');
  return {
    invited: invited.length,
    alreadyMember: rows.filter((row) => row.outcome === 'already_member').length,
    invalid: rows.filter((row) => row.outcome === 'invalid_email').length,
    failed: rows.filter((row) => row.outcome === 'failed').length,
    // An empty batch has delivered nothing, and must not read as "all delivered".
    delivered: invited.length > 0 && invited.every((row) => row.delivered),
  };
}

/** True when the dialog must stay open so the operator can read the failures. */
export function hasInviteFailures(rows: readonly InviteAddressResult[]): boolean {
  return rows.some((row) => row.outcome !== 'invited');
}
