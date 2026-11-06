import type { MessageGroupWire, MessageItemWire } from './wire';

/** Preserve error content and the server failure code on assistant messages. */
export function resolveAssistantFailureFields(
  messageGroup: MessageGroupWire,
  meta: MessageGroupWire['meta'],
  isError: boolean,
  messageItems: readonly MessageItemWire[],
) {
  const exception = isError ? meta?.error || messageGroup.content || messageItems[0]?.item_details?.content : undefined;
  return {
    ...(exception !== undefined ? { exception } : {}),
    ...(isError && typeof meta?.error_code === 'string' ? { failureCode: meta.error_code } : {}),
    ...(isError ? failureCapField(meta) : {}),
  };
}

/** The cap a CAP_REACHED response stopped at, when the server named one. */
function failureCapField(meta: MessageGroupWire['meta']): { readonly failureCap?: string } {
  const cap = meta?.cap;
  return typeof cap === 'string' && cap !== '' ? { failureCap: cap } : {};
}
