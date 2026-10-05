/**
 * Pure form logic for the native client dialog (`AdminNativeClientDialog`).
 *
 * The SERVER validates every redirect URI (`internal/nativeauth/registry.go`
 * `ValidateClient`): a private-use reverse-domain scheme
 * (`com.example.app:/oauth/callback`) or a loopback address registered without
 * a port (`http://127.0.0.1/callback`). This module does not restate those
 * rules — a client copy of them would drift the first time the server
 * tightened one. It does two things only: shape the text the operator typed
 * into the draft, and place each of the server's per-field reasons beside the
 * field it names (`redirect_uris[2]` → the third line).
 */
import type { NativeClient } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { NATIVE_CLIENT_SOURCE_FILE, type NativeClientDraft } from './api/adminNativeClientsApi';

/** What the dialog's inputs hold. Redirect URIs are one per line. */
export interface NativeClientForm {
  readonly clientId: string;
  readonly displayName: string;
  readonly redirectUris: string;
  readonly enabled: boolean;
  readonly minClientVersion: string;
}

export function initialNativeClientForm(editing: NativeClient | undefined): NativeClientForm {
  return {
    clientId: editing?.client_id ?? '',
    displayName: editing?.display_name ?? '',
    redirectUris: (editing?.redirect_uris ?? []).join('\n'),
    enabled: editing?.enabled ?? true,
    minClientVersion: editing?.min_client_version ?? '',
  };
}

/** One URI per non-blank line, trimmed. The server refuses duplicates itself. */
export function splitRedirectUris(text: string): string[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '');
}

export function nativeClientDraft(form: NativeClientForm): NativeClientDraft {
  return {
    clientId: form.clientId.trim(),
    displayName: form.displayName.trim(),
    redirectUris: splitRedirectUris(form.redirectUris),
    enabled: form.enabled,
    minClientVersion: form.minClientVersion.trim(),
  };
}

/** The server's reasons, placed by field. */
export interface NativeClientFieldErrors {
  readonly clientId?: string;
  readonly displayName?: string;
  readonly minClientVersion?: string;
  /** Reasons about the list as a whole, and per line (`2: …`), in order. */
  readonly redirectUris: readonly string[];
  /** Keys this dialog has no field for, so they are never silently dropped. */
  readonly other: readonly string[];
}

const URI_INDEX = /^redirect_uris\[(\d+)\]$/;

/**
 * Places the 422's `reasons` map. A per-URI reason is prefixed with the URI it
 * refused, so the operator does not count lines to find it.
 */
export function placeNativeClientReasons(
  reasons: Readonly<Record<string, string>>,
  uris: readonly string[],
): NativeClientFieldErrors {
  const redirectUris: string[] = [];
  const other: string[] = [];
  let clientId: string | undefined;
  let displayName: string | undefined;
  let minClientVersion: string | undefined;

  for (const key of Object.keys(reasons).sort()) {
    const reason = reasons[key] ?? '';
    const indexed = URI_INDEX.exec(key);
    if (key === 'client_id') clientId = reason;
    else if (key === 'display_name') displayName = reason;
    else if (key === 'min_client_version') minClientVersion = reason;
    else if (key === 'redirect_uris') redirectUris.unshift(reason);
    else if (indexed !== null) {
      const uri = uris[Number(indexed[1])];
      redirectUris.push(uri === undefined ? reason : `${uri}: ${reason}`);
    } else other.push(`${key}: ${reason}`);
  }

  return {
    ...(clientId === undefined ? {} : { clientId }),
    ...(displayName === undefined ? {} : { displayName }),
    ...(minClientVersion === undefined ? {} : { minClientVersion }),
    redirectUris,
    other,
  };
}

export const NO_FIELD_ERRORS: NativeClientFieldErrors = { redirectUris: [], other: [] };

/** A client the editor may change: one held in the database layer. */
export function isEditableNativeClient(client: NativeClient): boolean {
  return client.source !== NATIVE_CLIENT_SOURCE_FILE;
}

/**
 * The two refusals the dialog makes BEFORE any request, both in Register mode.
 *
 *  - A blank id would PUT `/administration/`, which no route matches: the
 *    server could only answer a bare 404, never "this field".
 *  - The PUT is an UPSERT. Registering an id a DATABASE row already holds
 *    would silently replace that client's name and redirect URIs, and every
 *    installed copy of the real app would fail its next sign-in. Editing that
 *    client is the way to change it. A FILE entry with the same id is not
 *    refused: shadowing it is the database layer's documented purpose.
 *
 * Returns `undefined` when the draft may be sent.
 */
export function registerRefusal(
  draft: Pick<NativeClientDraft, 'clientId'>,
  clients: readonly NativeClient[],
): NativeClientFieldErrors | undefined {
  if (draft.clientId === '') {
    return {
      ...NO_FIELD_ERRORS,
      clientId: t('pages.admin.nativeClients.dialog.clientIdRequired', 'Enter the client ID the app sends.'),
    };
  }
  const taken = clients.some((client) => client.client_id === draft.clientId && isEditableNativeClient(client));
  if (taken) {
    return {
      ...NO_FIELD_ERRORS,
      clientId: t(
        'pages.admin.nativeClients.dialog.clientIdTaken',
        'A client with this ID is already registered. Edit it instead.',
      ),
    };
  }
  return undefined;
}
