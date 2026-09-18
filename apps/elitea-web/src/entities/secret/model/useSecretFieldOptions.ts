/**
 * Builds the `secrets` configuration that `shared/ui`'s `SecretField` takes
 * as a prop — the option list, the refresh action, the permission flag and
 * the "Create new secret" shortcut.
 *
 * WHY THIS EXISTS (#441). `SecretField` is a `shared/ui` component, so rule
 * R-L1 forbids it to read Redux, an entity query or a permission list of its
 * own. The port therefore moved all four to caller-supplied props
 * (`SecretField.tsx`'s doc comment records the decision). No caller supplied
 * them. `secrets` was `undefined` in every production renderer, so the field
 * rendered as a plain masked text box, the mode toggle never appeared, the
 * saved-secret picker never appeared, and the "Create new secret" entry
 * inside that picker was off for every user — an administrator included. No
 * grant of `configuration.secrets.secret.create` could turn it on, which is
 * what parity item PERM-010 promises.
 *
 * This hook is that missing caller half, written once. It lives in
 * `entities/` because three different layers need it — `features/toolkits`,
 * `pages/credentials` and `widgets/llm-model-selector` — and
 * `no-sideways-features` forbids one feature slice to import another.
 *
 * MOUNT COST. Both queries run only where a secret field really renders.
 * Each consumer calls this hook from a component that mounts on the secret
 * path alone, never from a parent that renders for every field kind.
 *
 * PROJECT ID. Resolved with `readPersistedProject()`, the same synchronous,
 * provider-free read `app/session-store.ts`'s own `resolveSelectedProjectId`
 * uses. The three `useSelectedProjectId` duplicates in `features/` read the
 * router context instead, which `entities/` must not depend on: a secret
 * field renders inside dialogs that unit tests mount without a router.
 */
import { useCallback, useMemo } from 'react';

import { usePermissionList } from '@/shared/api/generated/auth/auth';
import type { Permission } from '@/shared/api/generated/model';
import { getConfig } from '@/shared/config';
import { t } from '@/shared/i18n';
import { normalizeBasename } from '@/shared/lib/basename';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { readPersistedProject } from '@/shared/lib/selectedProjectPersistence';
import type { SecretFieldSecretsOptions, SecretOption } from '@/shared/ui/SecretField';

import { useListSecretsQuery } from '../api/secretApi';
import type { Secret } from './types';

/**
 * The settings route that creates a secret, plus the `createSecret=1` flag
 * `routes/_shell/settings/secrets.tsx` validates (parity item PARAM-060).
 */
export const SECRETS_SETTINGS_PATH = '/settings/secrets?createSecret=1';

/**
 * `{{secret.NAME}}` — the reference syntax the backend expands, and the
 * shape `SecretField`'s own `SECRET_REFERENCE_RE` matches. The option VALUE
 * is the reference, not the bare name, because the value is what the form
 * stores.
 */
export function secretReference(name: string): string {
  return `{{secret.${name}}}`;
}

/**
 * Pure href builder, unit-tested directly.
 *
 * `basePath` mirrors `app/providers/basename.ts`'s `getAppBasename()`, which
 * `entities/` may not import (R-L1). Pass `''` for a dev build, where the
 * router mounts at the root.
 */
export function buildSecretsSettingsHref(basePath: string): string {
  return `${normalizeBasename(basePath)}${SECRETS_SETTINGS_PATH}`;
}

/** Same two branches as `getAppBasename()`: root-relative in dev, the configured base URI otherwise. */
function resolveSecretsSettingsHref(): string {
  if (import.meta.env.DEV) return buildSecretsSettingsHref('');
  const result = getConfig();
  return buildSecretsSettingsHref(result.status === 'ok' ? result.config.vite_base_uri : '');
}

/** Names only — the list endpoint never returns a plaintext value. */
export function toSecretOptions(secrets: readonly Secret[]): SecretOption[] {
  return secrets.map((secret) => ({ label: secret.name, value: secretReference(secret.name) }));
}

/**
 * #925/ELITEA-1069,1074: the scope-aware "Create new secret" wording, pure so
 * it is unit-testable without mounting the hook. `undefined` means "the
 * caller does not know the project's scope" — kept distinct from either
 * scope so `SecretField.tsx`'s own generic fallback renders instead of a
 * guess.
 */
export function secretCreateLabel(isTeamProject: boolean | undefined): string | undefined {
  if (isTeamProject === undefined) return undefined;
  return isTeamProject
    ? t('entities.secret.model.createLabelProject', 'New Project Secret')
    : t('entities.secret.model.createLabelPrivate', 'New Private Secret');
}

/** The permission names this surface reads, resolved from one permission list. */
export function readSecretFieldGrants(list: readonly Permission[] | undefined): { canList: boolean; canCreate: boolean } {
  const granted = new Set((list ?? []).filter((entry) => entry.enabled).map((entry) => entry.name));
  return { canList: granted.has(PERMISSIONS.secrets.list), canCreate: granted.has(PERMISSIONS.secrets.create) };
}

export interface UseSecretFieldOptionsParams {
  /**
   * #925/ELITEA-1069,1074: when the caller knows the project's scope, the
   * "Create new secret" option becomes scope-aware — "New Project Secret" /
   * "New Private Secret" — matching the CREDENTIAL picker's own
   * `CredentialCreateLabel`. Omitted (the default), the label stays the
   * generic fallback `SecretField.tsx` itself renders when `createLabel` is
   * absent — never a false claim about a scope this caller doesn't know.
   */
  readonly isTeamProject?: boolean;
}

export function useSecretFieldOptions(params: UseSecretFieldOptionsParams = {}): SecretFieldSecretsOptions {
  const { isTeamProject } = params;
  const projectId = readPersistedProject()?.id ?? '';

  const permissionQuery = usePermissionList(projectId, { query: { enabled: projectId !== '' } });
  // The declared type carries the error-envelope variant, which never
  // reaches here: `eliteaFetch` throws instead of resolving with it. Same
  // defensive cast `features/settings/lib/secrets/useSecretPermissions.ts`
  // makes on the same query.
  const grants = readSecretFieldGrants(permissionQuery.data?.data as Permission[] | undefined);

  // `canList` gates the QUERY, exactly as it does on the secrets settings
  // page: with no list grant the app must not ask the server at all.
  const listQuery = useListSecretsQuery(projectId, { enabled: projectId !== '' && grants.canList });
  const { refetch } = listQuery;

  const onCreate = useCallback(() => {
    // A new tab, not an in-app navigation: the field renders inside a form
    // the user is part-way through, and routing away from it would drop
    // every unsaved edit. This is the baseline's own `window.open` shape.
    window.open(resolveSecretsSettingsHref(), '_blank', 'noopener,noreferrer');
  }, []);

  const onRefresh = useCallback(() => {
    void refetch();
  }, [refetch]);

  const options = useMemo(() => toSecretOptions(listQuery.data ?? []), [listQuery.data]);

  const createLabel = useMemo(() => secretCreateLabel(isTeamProject), [isTeamProject]);

  return useMemo(
    () => ({ options, canCreate: grants.canCreate, onCreate, onRefresh, ...(createLabel !== undefined ? { createLabel } : {}) }),
    [options, grants.canCreate, onCreate, onRefresh, createLabel],
  );
}
