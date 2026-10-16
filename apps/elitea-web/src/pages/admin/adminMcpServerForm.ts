/**
 * The pre-built MCP server dialog's data rules, separated from its rendering.
 *
 * These are the parts worth reading and testing on their own: the key
 * derivation that has to match the server character for character, the
 * tri-state that decides whether a credential survives a save, and the
 * client-side checks. `./AdminMcpServerDialog.tsx` is the controls.
 */
import { dump, load } from 'js-yaml';

import { t } from '@/shared/i18n';

import type { AdminMcpServer } from './api/adminMcpServersApi';

/**
 * The server's `NormalizeCatalogueKey`, reproduced so the operator sees the key
 * their display name will be stored under before they save it.
 *
 * It must stay identical to `internal/mcpregistry.NormalizeCatalogueKey`,
 * INCLUDING the order: lowercase, spaces to underscores, THEN trim, then strip
 * a leading `mcp_`. Trimming first would produce a different key from the one
 * the server stores, and the lookup a toolkit does at run time would miss.
 */
export function normalizeCatalogueKey(name: string): string {
  if (name === '') return '';
  const normalized = name.toLowerCase().replaceAll(' ', '_').trim();
  return normalized.startsWith('mcp_') ? normalized.slice(4) : normalized;
}

/**
 * The dialog's fields as ONE state object.
 *
 * Nine separate `useState` hooks made the reset effect a list of nine setters,
 * each with its own fallback — nine chances for a field to be reset from the
 * wrong source, and no single place to read what "opening on this entry" means.
 */
export interface McpServerForm {
  readonly displayName: string;
  readonly url: string;
  readonly baseUrl: string;
  readonly clientId: string;
  readonly tokenUrl: string;
  readonly clientSecret: string;
  readonly clearSecret: boolean;
  readonly timeout: string;
  readonly headersYaml: string;
  readonly configSchemaYaml: string;
  readonly enabled: boolean;
}

/** A blank form: what "Add MCP server" opens on. */
const EMPTY_FORM: McpServerForm = {
  displayName: '',
  url: '',
  baseUrl: '',
  clientId: '',
  tokenUrl: '',
  clientSecret: '',
  clearSecret: false,
  timeout: '',
  headersYaml: '{}\n',
  configSchemaYaml: 'properties: {}\n',
  enabled: true,
};

function dumpYamlMapping(value: Readonly<Record<string, unknown>> | undefined): string {
  const mapping = value == null || Object.keys(value).length === 0 ? {} : value;
  return dump(mapping, { lineWidth: -1, noRefs: true });
}

/**
 * What the dialog shows when it opens on an entry, or on nothing.
 *
 * `clientSecret` is ALWAYS empty and `clearSecret` always false, whatever the
 * entry holds: the server never sends the secret, so there is nothing to
 * pre-fill with, and "untouched" must mean "leave the sealed one alone".
 *
 * A zero timeout renders as an EMPTY field. Zero means "not configured" on the
 * server, so the caller's own default applies; showing a literal 0 would read
 * as a zero-second timeout the operator had chosen.
 */
export function initialForm(editing: AdminMcpServer | undefined): McpServerForm {
  if (editing === undefined) return EMPTY_FORM;
  return {
    displayName: editing.display_name,
    url: editing.url,
    baseUrl: editing.base_url,
    clientId: editing.client_id,
    tokenUrl: editing.token_url ?? '',
    clientSecret: '',
    clearSecret: false,
    timeout: editing.timeout > 0 ? String(editing.timeout) : '',
    headersYaml: dumpYamlMapping(editing.headers),
    configSchemaYaml: dumpYamlMapping(editing.config_schema ?? { properties: {} }),
    enabled: editing.enabled,
  };
}

/** Parse one operator-authored YAML mapping without accepting arrays or scalars. */
export function parseMcpYamlMapping(source: string): Record<string, unknown> {
  const parsed: unknown = load(source.trim() === '' ? '{}' : source);
  if (parsed == null) return {};
  if (typeof parsed !== 'object' || Array.isArray(parsed)) {
    throw new Error('the YAML document must be a mapping');
  }
  return parsed as Record<string, unknown>;
}

/** Parse the header mapping and reject values that the Main wire type cannot accept. */
export function parseMcpHeadersYaml(source: string): Record<string, string> {
  const mapping = parseMcpYamlMapping(source);
  if (Object.values(mapping).some((value) => typeof value !== 'string')) {
    throw new Error('header values must be strings');
  }
  return mapping as Record<string, string>;
}

/**
 * Turns the two secret controls into the tri-state the server reads.
 *
 * `undefined` ⇒ omit the field, leaving the sealed secret alone. `''` ⇒ clear
 * it. A value ⇒ re-seal it.
 *
 * This is the one place on this screen where a wrong answer silently and
 * permanently destroys a credential, and it is three lines that look obviously
 * right — which is exactly why it is a named function with its own test.
 */
export function resolveSecretForSave(typed: string, clear: boolean): string | undefined {
  // Clearing WINS over a typed value. The checkbox disables the field, so a
  // keystroke left over from before it was ticked must not resurrect a secret
  // the operator chose to remove.
  if (clear) return '';
  if (typed === '') return undefined;
  return typed;
}

/**
 * The dialog's client-side checks, as one pure function.
 *
 * Returns the message to show, or `undefined` when the draft is acceptable.
 * These are a COURTESY: the server refuses each of them independently
 * (`internal/api/v2/admin/mcp_prebuilt.go`), and its refusal is what the dialog
 * renders when the two disagree.
 */
export function validateDraft(
  displayName: string,
  timeout: string,
  headersYaml: string,
  configSchemaYaml: string,
  isEdit: boolean,
  existingKeys: ReadonlySet<string>,
): string | undefined {
  const name = displayName.trim();
  if (name === '') {
    return t('pages.admin.mcpServers.dialog.error.nameRequired', 'Display name is required.');
  }
  const key = normalizeCatalogueKey(name);
  if (key === '') {
    return t(
      'pages.admin.mcpServers.dialog.error.keyEmpty',
      'That name has no usable catalogue key. Include a letter or a digit.',
    );
  }
  if (!isEdit && existingKeys.has(key)) {
    return t(
      'pages.admin.mcpServers.dialog.error.duplicate',
      'A catalogued server already uses that key.',
    );
  }
  const seconds = timeout.trim() === '' ? 0 : Number(timeout);
  if (!Number.isInteger(seconds) || seconds < 0) {
    return t(
      'pages.admin.mcpServers.dialog.error.timeout',
      'Timeout must be a whole number of seconds, or empty for the default.',
    );
  }
  try {
    parseMcpHeadersYaml(headersYaml);
  } catch {
    return t('pages.admin.mcpServers.dialog.error.headersYaml', 'Headers must be a valid YAML mapping.');
  }
  try {
    parseMcpYamlMapping(configSchemaYaml);
  } catch {
    return t(
      'pages.admin.mcpServers.dialog.error.configSchemaYaml',
      'Project parameter schema must be a valid YAML mapping.',
    );
  }
  return undefined;
}

/**
 * Courtesy check of the pinned token endpoint, mirroring the server's rule
 * (`mcpregistry.ValidatePrebuiltTokenURL`): an exact URL, https (http only to
 * loopback), no placeholder. The server refuses the same cases independently.
 *
 * A templated server URL that carries a client secret needs a pin: the secret
 * is only ever sent to the pinned endpoint (or to one discovered from an exact
 * server URL), so without it the secret could never be used.
 */
export function validateTokenUrl(tokenUrl: string, serverUrl: string, willHaveSecret: boolean): string | undefined {
  const pin = tokenUrl.trim();
  if (pin === '') {
    if (willHaveSecret && /[{}]/.test(serverUrl)) {
      return t(
        'pages.admin.mcpServers.dialog.error.tokenUrlRequired',
        'A server URL with placeholders and a client secret needs a token endpoint. Enter the OAuth token URL the secret may be sent to.',
      );
    }
    return undefined;
  }
  return exactEndpoint(pin)
    ? undefined
    : t(
        'pages.admin.mcpServers.dialog.error.tokenUrl',
        'The token endpoint must be an exact https URL without placeholders.',
      );
}

function isLoopbackHost(hostname: string): boolean {
  return hostname === 'localhost' || hostname.endsWith('.localhost') || hostname === '127.0.0.1' || hostname === '[::1]';
}

/** One absolute URL: no placeholder, whitespace, credentials, fragment or dot segment (checked on the raw text, since `URL` resolves them away). */
function exactEndpoint(pin: string): boolean {
  if (/[{}\s\\]/.test(pin) || pin.length > 4096) return false;
  if (pin.split('?')[0]?.split('/').some((segment) => segment === '.' || segment === '..')) return false;
  let parsed: URL;
  try {
    parsed = new URL(pin);
  } catch {
    return false;
  }
  const schemeOk = parsed.protocol === 'https:' || (parsed.protocol === 'http:' && isLoopbackHost(parsed.hostname));
  return schemeOk && parsed.username === '' && parsed.password === '' && parsed.hash === '';
}

/** The catalogue key the entry is, or will be, stored under. */
export function keyHelperText(fixedKey: string | undefined, displayName: string): string {
  if (fixedKey !== undefined) {
    return t('pages.admin.mcpServers.dialog.keyFixed', 'Catalogue key: {{key}} (cannot change)', {
      key: fixedKey,
    });
  }
  return t('pages.admin.mcpServers.dialog.keyDerived', 'Catalogue key: {{key}}', {
    key: normalizeCatalogueKey(displayName.trim()) || '—',
  });
}
