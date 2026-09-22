/**
 * The ONE rendering of a tool's input/output payload.
 *
 * Two views show the same column — the chat pane's tool modal and the run
 * history's trace step — and they disagreed: the trace round-tripped the
 * payload through `JSON.parse`/`JSON.stringify` to pretty-print it, which
 * rewrites every number the IEEE-754 double cannot hold. A bigint identifier
 * came back with trailing zeros and `1e400` came back as `null`, while the
 * chat pane showed the same row verbatim (#990 review 6).
 *
 * So the rule is the chat pane's, and it is the conservative one: a value that
 * is ALREADY a string is what the server sent, and is shown exactly as sent;
 * anything else is a structure this client itself holds, so pretty-printing it
 * invents nothing.
 */
export function toolPayloadText(value: unknown): string {
  if (value === undefined || value === null) return '';
  if (typeof value === 'string') return value;
  return JSON.stringify(value, null, 2);
}
