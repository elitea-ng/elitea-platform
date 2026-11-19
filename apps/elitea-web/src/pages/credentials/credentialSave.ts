/**
 * pages/credentials/credentialSave.ts — the create-vs-update dispatch and error mapping of
 * the credential form, split out of `./useCredentialFormController.ts` (which sat at its
 * 400-line budget) when the Microsoft sign-in needed a save of its own.
 */
import { extractInformationFromCredentialError } from '@/features/credentials';
import type { ConfigSchemaNode, useCreateConfiguration, useUpdateConfiguration } from '@/features/credentials';

/** The part of the form's mode a save reads: which row it writes. Structural, so this file does not import the controller that imports it. */
export interface SaveMode {
  readonly kind: 'create' | 'edit';
  readonly configId?: string | undefined;
}

/**
 * `useCreateCredential.jsx`'s title fallback: `settings.elitea_title ||
 * \`${type}_${timestamp}\`` — same for update (`updated_configurartion_...`,
 * baseline's own typo not reproduced here since this port shares one title
 * builder for both paths; the value is a fallback default a user virtually
 * always overrides).
 *
 * `label` is a SECOND fallback this port adds ahead of the generated
 * timestamp, for the one case baseline's own `settings?.elitea_title`
 * genuinely has none to read: creating a brand-new credential, where no
 * stable key exists yet and the generic ToolBase form (out of this port's
 * scope, see `CredentialForm.tsx`'s disclosed-scope note) would otherwise
 * have slugified whatever the user just typed as the starting key. On
 * *edit*, `eliteaTitle` is seeded from the server and is basically never
 * empty in practice, so this fallback essentially never fires there —
 * `label` is deliberately still second (never first): a stable
 * non-empty `eliteaTitle` always wins, so an edit-save can never let a
 * rename creep into the lookup key other domains resolve this credential
 * by (finding A7-pages/1 — see `performSave`'s own `eliteaTitle` doc
 * comment).
 */
function buildTitle(eliteaTitle: string | undefined, label: string, type: string): string {
  if (eliteaTitle && eliteaTitle.trim() !== '') return eliteaTitle;
  if (label && label.trim() !== '') return label;
  return `${type}_${new Date().toISOString().slice(0, 19).replace(/[:-]/g, '')}`;
}

/** Maps a thrown `EliteaApiError` (see `shared/api/generated/mutator.ts`) onto the `{data:{message,field}}` shape `extractInformationFromCredentialError` expects. `unknown` in, since a caught value is never narrowed by TypeScript. */
function toCredentialApiError(error: unknown): { readonly data?: { readonly message?: unknown; readonly field?: unknown } } {
  if (typeof error !== 'object' || error === null || !('failure' in error)) return {};
  const failure = (error as { failure?: unknown }).failure;
  if (typeof failure !== 'object' || failure === null || !('body' in failure)) return {};
  const body = (failure as { body?: unknown }).body;
  if (typeof body !== 'object' || body === null) return {};
  const record = body as Record<string, unknown>;
  return { data: { message: record['error'] ?? record['message'], field: record['field'] } };
}

export type SaveOutcome =
  | { readonly status: 'ok'; readonly id?: string; readonly uuid?: string }
  | { readonly status: 'fieldErrors'; readonly errors: Readonly<Record<string, string>> }
  | { readonly status: 'genericError' };

export interface PerformSaveParams {
  readonly mode: SaveMode;
  readonly projectId: string;
  readonly type: string;
  /** The freely-editable display name — always submitted as `label`, unmodified, exactly as typed (`hooks/credentials/useUpdateCredential.jsx`'s `getRequestBody`: `label: settings.label || ''`). */
  readonly label: string;
  /**
   * The internally-stable lookup key other domains resolve this credential
   * by. `undefined` on create (none exists yet — `buildTitle` derives one
   * from `label`, matching `hooks/credentials/useCreateCredential.jsx`'s
   * `settings?.elitea_title || \`${toolType}_${timestamp}\`` with no prior
   * value to fall back to). On edit this is the value loaded from the
   * server (seeded once by `useFormSeeding`, never re-derived from
   * `label`) — submitted as-is, matching `useUpdateCredential.jsx`'s own
   * `settings?.elitea_title || fallback-generated` (never `settings.label`).
   */
  readonly eliteaTitle: string | undefined;
  readonly shared: boolean;
  readonly data: Readonly<Record<string, unknown>>;
  readonly schemaProperties: Readonly<Record<string, ConfigSchemaNode>>;
  readonly createConfiguration: ReturnType<typeof useCreateConfiguration>;
  readonly updateConfiguration: ReturnType<typeof useUpdateConfiguration>;
}

/** The create-vs-update dispatch + error-mapping, isolated from the hook so its own complexity is measured separately (§3.5). */
export async function performSave(params: PerformSaveParams): Promise<SaveOutcome> {
  const { mode, projectId, type, label, eliteaTitle, shared, data, schemaProperties, createConfiguration, updateConfiguration } = params;
  const body = { elitea_title: buildTitle(eliteaTitle, label, type), label, data, shared };
  try {
    // Captured on both branches (not just create): an edit-save's response
    // carries the same row's id, and the caller (`save` below) treats the two
    // uniformly — see `onSaved`'s doc comment on why the edit route also
    // wants this to reveal the row's section on return.
    const saved = mode.kind === 'edit' && mode.configId
      ? await updateConfiguration.mutateAsync({ projectId, configId: mode.configId, body })
      : await createConfiguration.mutateAsync({ projectId, body: { ...body, type } });
    const uuid = saved.uuid ?? saved.uid;
    return {
      status: 'ok',
      ...(saved.id !== undefined ? { id: String(saved.id) } : {}),
      ...(typeof uuid === 'string' && uuid !== '' ? { uuid } : {}),
    };
  } catch (error) {
    const { newErrors } = extractInformationFromCredentialError({ error: toCredentialApiError(error), schemaProperties, settings: data });
    if (Object.keys(newErrors).length > 0) return { status: 'fieldErrors', errors: newErrors };
    return { status: 'genericError' };
  }
}

