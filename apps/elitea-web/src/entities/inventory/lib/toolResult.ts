/**
 * Reading an Inventory tool's answer out of the SPI's terminal poll.
 *
 * THE ANSWER IS THREE LAYERS DEEP, and every layer is a real encoding rather
 * than an accident, so all three have to be peeled in order:
 *
 *   1. `poll.result` is a JSON STRING holding an ARRAY of result objects —
 *      `spi.Completed` marshals `[]ResultObject` and stores the text
 *      (services/elitea-subapp-host/internal/spi/runner.go:35-47).
 *   2. Exactly one of those objects has `object_type: "message"` and
 *      `result_target: "response"`; the rest are artifacts bound for a bucket
 *      (`graph.json`, `sources_status.json`, the ingestion checkpoint). The
 *      message's `data` is the tool's own `result` string.
 *   3. For a tool called with `output_format: "json"` that string is itself a
 *      JSON document. Called without it, the same field is MARKDOWN.
 *
 * Reading only layer 1 gives a screen the whole envelope as its data; reading
 * layers 1 and 2 and forgetting 3 gives it a string that renders as a wall of
 * escaped JSON. Both look like a bug in the provider and neither is.
 *
 * NOTHING HERE THROWS ON A SHAPE IT DOES NOT KNOW. A provider that answers a
 * plain string, or an object instead of an array, is answering — the reader
 * degrades to the text it can see. What throws is the caller's business: a
 * document with no rows is an empty screen, and a REFUSAL is a poll with
 * status Error, which `terminalOutcome` reports before this module is reached.
 */

/** One entry of the SPI result list. Only two of its fields are read here. */
interface ResultObject {
  readonly object_type?: unknown;
  readonly result_target?: unknown;
  readonly data?: unknown;
  readonly name?: unknown;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** JSON.parse that answers `undefined` instead of throwing. */
function parseJson(text: string): unknown {
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return undefined;
  }
}

/**
 * Layer 1 + 2: the tool's own `result` text out of a terminal poll's `result`.
 *
 * A poll whose result is not the marshalled list — a provider that answers the
 * message text directly — is returned unchanged, because that text IS the
 * answer under any reading.
 */
export function toolResultText(pollResult: string | undefined): string {
  const raw = (pollResult ?? '').trim();
  if (raw === '') return '';
  if (!raw.startsWith('[')) return raw;

  const objects = parseJson(raw);
  if (!Array.isArray(objects)) return raw;

  for (const entry of objects as readonly ResultObject[]) {
    if (!isRecord(entry)) continue;
    if (entry.object_type !== 'message') continue;
    return typeof entry.data === 'string' ? entry.data : '';
  }
  // A terminal body with artifacts and no message: the tool answered, and what
  // it answered is in the bucket. An empty string is the honest reading.
  return '';
}

/** One artifact the terminal body carried, named by the key it was stored under. */
export interface ToolArtifact {
  readonly name: string;
  readonly objectType: string;
}

/**
 * Layer 1: the artifacts a terminal body carried.
 *
 * `run_ingestion` is the only tool that produces any, and the SCREEN needs
 * them: the ingestion checkpoint is an artifact key and nothing else reports
 * that the checkpoint was written. Their `data` is deliberately not returned —
 * the object is in the bucket, and a graph inlined into a poll body would be
 * megabytes of string the browser has no use for.
 */
export function toolArtifacts(pollResult: string | undefined): readonly ToolArtifact[] {
  const raw = (pollResult ?? '').trim();
  if (!raw.startsWith('[')) return [];
  const objects = parseJson(raw);
  if (!Array.isArray(objects)) return [];

  const artifacts: ToolArtifact[] = [];
  for (const entry of objects as readonly ResultObject[]) {
    if (!isRecord(entry)) continue;
    if (entry.result_target !== 'artifact') continue;
    const name = typeof entry.name === 'string' ? entry.name : '';
    if (name === '') continue;
    artifacts.push({
      name,
      objectType: typeof entry.object_type === 'string' ? entry.object_type : '',
    });
  }
  return artifacts;
}

/**
 * Layer 3: the JSON document a `output_format: "json"` call answered.
 *
 * `undefined` for a tool that answered markdown, or for a document that is not
 * an object. The caller decides what an unreadable document means; every
 * reader in this slice treats it as "no rows", which is what the screen would
 * show for an empty graph as well — the two are the same to a user, and the
 * difference is in the network tab.
 */
export function toolResultDocument(pollResult: string | undefined): Record<string, unknown> | undefined {
  const text = toolResultText(pollResult).trim();
  if (text === '') return undefined;
  const document = parseJson(text);
  return isRecord(document) ? document : undefined;
}

/**
 * The sentence to SHOW for a refusal.
 *
 * A failure carries the envelope too. `spi.ToolError` marshals
 * `[]ResultObject{Message(text)}` into the same `result` field a success uses
 * (services/elitea-subapp-host/internal/spi/errors.go:130-147), so the
 * "message" a caller reads off a terminal poll is a JSON array, not a
 * sentence. Rendering it unpeeled puts `[{"object_type":"message",…}]` in the
 * error banner — the failure is reported, and the reason is unreadable.
 *
 * The raw text is the fallback rather than the empty string: a refusal that
 * came from the FACADE (a 403 for a source this toolkit does not own) is a
 * plain sentence with no envelope around it, and losing it would leave a
 * banner with nothing in it.
 */
export function toolErrorText(message: string | undefined): string {
  const peeled = toolResultText(message).trim();
  if (peeled !== '') return peeled;
  return (message ?? '').trim();
}
