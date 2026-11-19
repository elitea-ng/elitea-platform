/**
 * Pure form logic for the Microsoft app dialog (`AdminMicrosoftAppDialog`).
 *
 * The SERVER validates everything that matters (the Entra host of the authority, the
 * GUID of the client id, the fixed permission lists, `.default` never in a ceiling):
 * `internal/microsoftapps/app.go` and `permissions.go`. This module does not restate
 * those rules; it shapes what the administrator typed into the request body, keeps
 * the secret write-only, and finds the line a refusal names.
 *
 * ## The secret has three states on the wire
 *
 * Absent keeps the sealed secret, an empty string clears it (the app becomes a
 * draft), a value seals it. A form that does not echo secrets must not erase the
 * credential, so an untouched secret field sends NOTHING.
 */
import type { MicrosoftApp, MicrosoftAppRequest, MicrosoftAppTypePermissions } from '@/shared/api/generated/model';

/** The five credential types a registry app can serve, in display order. */
export const MICROSOFT_APP_TYPES = ['outlook', 'teams', 'ms_graph', 'dataverse', 'sharepoint'] as const;

/** Types whose ceiling is typed as free text (Graph permission names). */
const FREE_TEXT_TYPES: ReadonlySet<string> = new Set(['ms_graph', 'sharepoint']);

/** Dataverse takes no ceiling: its one scope comes from the credential's org URL. */
const NO_CEILING_TYPE = 'dataverse';

export type MicrosoftAppProjectScope = 'all' | 'selected';

/** What the dialog's inputs hold. */
export interface MicrosoftAppForm {
  readonly name: string;
  readonly description: string;
  readonly clientId: string;
  readonly authority: string;
  readonly types: readonly string[];
  /** Fixed-list types (Outlook, Teams): the ticked permissions. */
  readonly ticked: Readonly<Record<string, readonly string[]>>;
  /** Free-text types (Microsoft Graph, SharePoint): permission names, space or comma separated. */
  readonly typed: Readonly<Record<string, string>>;
  readonly autoRefresh: boolean;
  readonly projectScope: MicrosoftAppProjectScope;
  /** Project ids, comma or space separated. */
  readonly projectIds: string;
  /** Write-only. Empty means the administrator typed nothing. */
  readonly secret: string;
  /** The administrator asked to remove the sealed secret (the app becomes a draft). */
  readonly clearSecret: boolean;
  /** yyyy-mm-dd, or empty. */
  readonly secretExpiresAt: string;
}

export function isFreeTextType(type: string): boolean {
  return FREE_TEXT_TYPES.has(type);
}

export function hasCeiling(type: string): boolean {
  return type !== NO_CEILING_TYPE;
}

const DEFAULT_AUTHORITY = 'https://login.microsoftonline.com/organizations';

/** An empty form for a new app: the least-privilege default of every type ticked. */
export function emptyMicrosoftAppForm(typeInfo: readonly MicrosoftAppTypePermissions[]): MicrosoftAppForm {
  const ticked: Record<string, readonly string[]> = {};
  for (const info of typeInfo) {
    if (!isFreeTextType(info.type) && hasCeiling(info.type)) ticked[info.type] = info.default;
  }
  return {
    name: '',
    description: '',
    clientId: '',
    authority: DEFAULT_AUTHORITY,
    types: [],
    ticked,
    typed: {},
    autoRefresh: true,
    projectScope: 'all',
    projectIds: '',
    secret: '',
    clearSecret: false,
    secretExpiresAt: '',
  };
}

/** The form of an existing app. The secret field starts empty: it is never read back. */
export function microsoftAppFormOf(app: MicrosoftApp, typeInfo: readonly MicrosoftAppTypePermissions[]): MicrosoftAppForm {
  const base = emptyMicrosoftAppForm(typeInfo);
  const ticked: Record<string, readonly string[]> = { ...base.ticked };
  const typed: Record<string, string> = {};
  for (const type of app.credential_types) {
    if (!hasCeiling(type)) continue;
    if (isFreeTextType(type)) {
      typed[type] = (app.scope_ceiling[type] ?? []).join(' ');
    } else {
      ticked[type] = app.effective_ceiling[type] ?? ticked[type] ?? [];
    }
  }
  return {
    ...base,
    name: app.name,
    description: app.description,
    clientId: app.client_id,
    authority: app.authority,
    types: app.credential_types,
    ticked,
    typed,
    autoRefresh: app.auto_refresh_token,
    projectScope: app.project_scope === 'selected' ? 'selected' : 'all',
    projectIds: app.project_ids.join(', '),
    secretExpiresAt: app.secret_expires_at === null ? '' : app.secret_expires_at.slice(0, 10),
  };
}

/** Space or comma separated words, without blanks. */
function splitWords(text: string): string[] {
  return text
    .split(/[\s,]+/)
    .map((word) => word.trim())
    .filter((word) => word !== '');
}

/** The project ids typed, or `undefined` when one is not a positive integer. */
export function parseProjectIds(text: string): number[] | undefined {
  const ids: number[] = [];
  for (const word of splitWords(text)) {
    if (!/^[1-9][0-9]*$/.test(word)) return undefined;
    ids.push(Number(word));
  }
  return ids;
}

function ceilingOf(form: MicrosoftAppForm): Record<string, string[]> {
  const ceiling: Record<string, string[]> = {};
  for (const type of form.types) {
    if (!hasCeiling(type)) continue;
    const names = isFreeTextType(type) ? splitWords(form.typed[type] ?? '') : [...(form.ticked[type] ?? [])];
    if (names.length > 0) ceiling[type] = names;
  }
  return ceiling;
}

function secretOf(form: MicrosoftAppForm): { secret?: string } {
  if (form.clearSecret) return { secret: '' };
  return form.secret.trim() === '' ? {} : { secret: form.secret };
}

/** A refusal the form can place before the request is sent. */
export interface MicrosoftAppFormRefusal {
  readonly field: 'project_ids';
}

/** The request body, or the local refusal that stops it. */
export function microsoftAppRequest(form: MicrosoftAppForm): MicrosoftAppRequest | MicrosoftAppFormRefusal {
  const projectIds = form.projectScope === 'selected' ? parseProjectIds(form.projectIds) : [];
  if (projectIds === undefined) return { field: 'project_ids' };
  return {
    name: form.name.trim(),
    description: form.description.trim(),
    client_id: form.clientId.trim(),
    authority: form.authority.trim(),
    credential_types: [...form.types],
    scope_ceiling: ceilingOf(form),
    auto_refresh_token: form.autoRefresh,
    project_scope: form.projectScope,
    project_ids: projectIds,
    secret_expires_at: form.secretExpiresAt === '' ? null : form.secretExpiresAt,
    ...secretOf(form),
  };
}

export function isFormRefusal(result: MicrosoftAppRequest | MicrosoftAppFormRefusal): result is MicrosoftAppFormRefusal {
  return 'field' in result && !('name' in result);
}

/** Ticks or unticks one name, keeping the server's list order. */
export function toggleName(current: readonly string[], offered: readonly string[], name: string): string[] {
  const next = new Set(current);
  if (next.has(name)) next.delete(name);
  else next.add(name);
  return offered.filter((candidate) => next.has(candidate));
}
