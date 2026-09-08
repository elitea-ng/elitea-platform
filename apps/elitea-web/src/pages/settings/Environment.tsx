/**
 * Environment settings section — fetches field definitions from the server,
 * renders each as an `EnvironmentFieldRow`, and flushes changes on blur.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/ui/environment/
 * EnvironmentSection.jsx`.
 *
 * Deviations from the baseline:
 *  - Uses `useSelectedProjectStore` instead of Redux hooks
 *  - Uses the handwritten `configurationsApi` instead of RTK Query slices
 *  - No `useToast` — errors surface inline on the field row itself (via
 *    `EnvironmentFieldRow`'s `error` prop) instead of a toast/snackbar;
 *    a real toast system is a future unit (see `ServicePrompts.tsx` for
 *    the same documented gap elsewhere in this settings area)
 *  - Edits are saved on blur (same as baseline) — no explicit save button
 *  - Creates a new configuration if none exists yet (same as baseline)
 *
 * The create-vs-update decision reads `currentConfig`, so anything that
 * hides an EXISTING row makes every blur write a NEW one. Three doors were
 * open into that, and all three are closed below:
 *  - a failed list read left `currentConfig` null with no error on screen.
 *  - this page writes `shared: true`, and the compat list handler returns
 *    shared rows in a SEPARATE `shared.items` bucket this page never read.
 *  - a blur with no edit still wrote, because the "unchanged" comparison
 *    compares against a value that is `undefined` while the row is hidden.
 */
import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AvailableConfigurationType, ConfigurationItem, ConfigurationsListResponse } from '@/shared/api/configurationsApi';
import { useCreateConfigurationMutation, useGetAvailableConfigurationsTypeQuery, useGetConfigurationsListQuery, useUpdateConfigurationMutation } from '@/shared/api/configurationsApi';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import type { EnvironmentFieldDefinition } from '@/features/settings';
import { environmentFeature } from '@/features/settings';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { useIsPublicProject } from '@/shared/lib/hooks/usePublicProjectId';
import { t } from '@/shared/i18n';
import { useSelectedProjectStore } from '@/widgets/app-shell';
import { usePermissionSet } from '@/widgets/sidebar';

const { ENVIRONMENT_FIELD_DEFAULTS, ENVIRONMENT_FIELD_ORDER, ENVIRONMENT_SECTION, buildFieldDefinition, parseFieldValue, validateFieldValue, EnvironmentFieldRow } = environmentFeature;

/* ── helpers ──────────────────────────────────────────────────────────── */

/*
 * [#71] The local duplicate of `parseFieldValue` that used to live here is
 * gone. Its own comment named the reason it existed — `pages/` may only reach
 * `features/settings` through the curated barrel (`no-deep-slice-import`),
 * "which does not currently re-export `parseFieldValue` (see this unit's fix
 * report for the barrel follow-up)". That follow-up is now done:
 * `environmentFeature` re-exports it, so this page uses the one canonical
 * implementation, which is also the one the baseline calls
 * (`EnvironmentFieldHelpers.parseFieldValue`, EnvironmentSection.jsx:122).
 * Leaving the copy in place was what made knip report the real helper as an
 * unused export.
 */

/** Extract a human-readable message from a save/restore failure. */
function extractErrorMessage(err: unknown, fallback: string): string {
  if (err instanceof EliteaApiError && err.failure.kind === 'http') {
    const body = err.failure.body;
    if (body !== null && typeof body === 'object') {
      const record = body as Record<string, unknown>;
      if (typeof record.error === 'string' && record.error) return record.error;
      if (typeof record.message === 'string' && record.message) return record.message;
    }
  }
  if (err instanceof Error && err.message) return err.message;
  return fallback;
}

/**
 * Render a raw config/default value (of otherwise-`unknown` type — the
 * server's `data` bag isn't statically typed) as a display string. Only
 * ever calls `String()` on values already narrowed to `string | number |
 * boolean` so it never risks `[object Object]`.
 */
function toDisplayString(value: unknown): string {
  if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') return String(value);
  return '';
}

/** Clear a single field's inline error, if it has one (no-op state update avoided). */
function clearFieldError(
  setFieldErrors: React.Dispatch<React.SetStateAction<Record<string, string>>>,
  fieldKey: string,
): void {
  setFieldErrors((prev) => {
    if (!(fieldKey in prev)) return prev;
    const next = { ...prev };
    delete next[fieldKey];
    return next;
  });
}

/**
 * Every configuration row the list response carries, own AND shared.
 *
 * This page saves with `shared: true`. The compat list handler
 * (`internal/api/v2/configurations/handler.go`) selects `items` with
 * `WHERE shared = false`. It returns the shared rows in a separate
 * `shared.items` bucket. Reading `items` alone therefore hid this page's own
 * saved row on a perfectly successful 200. Every blur then took the CREATE
 * branch and added another single-key row.
 *
 * `ConfigurationsListResponse` does not declare the `shared` bucket. A local
 * narrowing here reads that bucket. Do not widen the shared API type from
 * this page.
 */
function listedConfigurations(data: ConfigurationsListResponse | undefined): readonly ConfigurationItem[] {
  const sharedItems = (data as { readonly shared?: { readonly items?: readonly ConfigurationItem[] } } | undefined)?.shared?.items ?? [];
  return [...(data?.items ?? []), ...sharedItems];
}

/**
 * Normalise a schema property map into ordered field definitions. Filters
 * `ENVIRONMENT_FIELD_ORDER` down to keys the backend schema actually
 * declares (matches the old app's `ENVIRONMENT_FIELD_ORDER.filter(key =>
 * key in dataProperties)`) instead of mapping over it unconditionally.
 */
function buildFields(
  availableTypes: AvailableConfigurationType[] | undefined,
): EnvironmentFieldDefinition[] {
  const raw = availableTypes?.find((item) => item.type === ENVIRONMENT_SECTION)?.config_schema;
  const schema = raw ?? {};
  const dataSchema = (schema?.properties as Record<string, unknown> | undefined)?.data as Record<string, unknown> | undefined;
  const dataProperties = (dataSchema?.properties as Record<string, unknown> | undefined) ?? {};
  return ENVIRONMENT_FIELD_ORDER.filter((key) => key in dataProperties).map((key) => {
    const fieldSchema = dataProperties[key] as Record<string, unknown> | undefined;
    const defaults = ENVIRONMENT_FIELD_DEFAULTS[key];
    return buildFieldDefinition(key, fieldSchema, defaults);
  });
}

/* ── component ────────────────────────────────────────────────────────── */

/**
 * Full environment settings section. Renders nothing when the public project
 * is not selected (environment settings only apply to the public project).
 */
export const Environment = memo(function Environment() {
  const projectId = useSelectedProjectStore((s) => s.project?.id ?? '');
  /*
   * The local `isPublicProject` that stood here read the image's
   * `VITE_PUBLIC_PROJECT_ID` through `shared/config`. That is one of the three
   * places the same project id was configured, and the only one the server
   * could not check. `useIsPublicProject` prefers the id elitea-main publishes
   * on `platform_settings` and keeps the image's copy as the fallback, so this
   * page and the server cannot disagree about which project is public.
   */
  const isPublic = useIsPublicProject(projectId);

  const permissions = usePermissionSet(isPublic ? projectId : undefined);
  const canEdit = permissions.has(PERMISSIONS.configuration.update);

  const { data, isLoading, isFetching, isError } = useGetConfigurationsListQuery(
    { projectId, section: ENVIRONMENT_SECTION, includeShared: false, pageSize: 100 },
    { enabled: !!projectId && isPublic },
  );

  const { data: availableTypes } = useGetAvailableConfigurationsTypeQuery(
    { section: ENVIRONMENT_SECTION },
    { enabled: !!projectId && isPublic },
  );

  const createMutation = useCreateConfigurationMutation(projectId);
  const updateMutation = useUpdateConfigurationMutation(projectId);

  const isBusy = isLoading || isFetching || createMutation.isPending || updateMutation.isPending;
  // A write is only safe once the read has SUCCEEDED. While it is loading or
  // failed, `currentConfig` is null for a reason that says nothing about
  // whether a row exists on the server.
  const canSave = canEdit && !isLoading && !isError;

  const schemaFields = useMemo(() => buildFields(availableTypes), [availableTypes]);

  // Current config from the server — from BOTH buckets, see `listedConfigurations`.
  const currentConfig = useMemo(() => {
    return listedConfigurations(data).find((item) => item.section === ENVIRONMENT_SECTION) ?? null;
  }, [data]);

  // Draft values — single state object keyed by field key
  const [draftValues, setDraftValues] = useState<Record<string, string>>({});
  /*
   * Field keys the user actually typed in since the last successful save.
   * `handleBlur` compares the parsed draft against `currentConfig?.data?.[key]`.
   * That value is `undefined` whenever no row is loaded, so every value
   * "differs". Merely tabbing THROUGH a field therefore issued a write. A ref, not state: a
   * blur must read the value the matching change already set, in the same
   * commit, and this never drives rendering.
   */
  const dirtyFieldsRef = useRef<Set<string>>(new Set());
  // Inline per-field error messages (validation failures + save/restore failures)
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({});

  // Track the previous currentConfig reference so we can detect server-side changes
  const prevConfigRef = useRef(currentConfig);

  // Sync drafts with server data when it arrives
  useEffect(() => {
    if (!schemaFields.length) return;

    const configChanged = prevConfigRef.current !== currentConfig;
    prevConfigRef.current = currentConfig;

    setDraftValues((prev) => {
      const allInitialized = schemaFields.every((field) => prev[field.key] !== undefined);
      if (allInitialized && !configChanged) return prev;

      const next = { ...prev };
      for (const field of schemaFields) {
        if (next[field.key] === undefined || configChanged) {
          next[field.key] = toDisplayString(currentConfig?.data?.[field.key] ?? field.defaultValue);
        }
      }
      return next;
    });
  }, [currentConfig, schemaFields]);

  // Flush pending edits on page reload by blurring the active input (same as
  // baseline `EnvironmentSection.jsx:77-84`) — otherwise a field the user is
  // still focused in when they reload/close the tab never gets its
  // onBlur-triggered save invoked.
  useEffect(() => {
    const handleBeforeUnload = () => {
      (document.activeElement as HTMLElement | null)?.blur();
    };
    window.addEventListener('beforeunload', handleBeforeUnload);
    return () => window.removeEventListener('beforeunload', handleBeforeUnload);
  }, []);

  const handleChange = useCallback((fieldKey: string, value: string) => {
    dirtyFieldsRef.current.add(fieldKey);
    setDraftValues((prev) => ({ ...prev, [fieldKey]: value }));
  }, []);

  const handleBlur = useCallback(
    async (fieldKey: string) => {
      if (!canSave) return;
      // Nothing was typed — a blur alone must never write.
      if (!dirtyFieldsRef.current.has(fieldKey)) return;

      const field = schemaFields.find((f) => f.key === fieldKey);
      if (!field) return;

      const rawValue = String(draftValues[fieldKey] || '').trim();
      const parsedValue = parseFieldValue(rawValue, field.type);
      const savedValue = currentConfig?.data?.[fieldKey];

      if (parsedValue === savedValue) {
        dirtyFieldsRef.current.delete(fieldKey);
        return;
      }

      const validationError = validateFieldValue(rawValue, field);
      if (validationError) {
        setFieldErrors((prev) => ({
          ...prev,
          [fieldKey]: t('shared.ui.settings.environment.validationError', '{{label}}: {{error}}', {
            label: field.label,
            error: validationError,
          }),
        }));
        setDraftValues((prev) => ({ ...prev, [fieldKey]: toDisplayString(savedValue ?? field.defaultValue) }));
        dirtyFieldsRef.current.delete(fieldKey);
        return;
      }

      try {
        if (currentConfig?.id) {
          await updateMutation.mutateAsync({
            configId: String(currentConfig.id),
            body: {
              label: currentConfig.label,
              shared: true,
              data: { ...currentConfig.data, [fieldKey]: parsedValue },
            },
          });
        } else {
          await createMutation.mutateAsync({
            elitea_title: ENVIRONMENT_SECTION,
            label: ENVIRONMENT_SECTION,
            type: ENVIRONMENT_SECTION,
            shared: true,
            data: { [fieldKey]: parsedValue },
          });
        }
        dirtyFieldsRef.current.delete(fieldKey);
        clearFieldError(setFieldErrors, fieldKey);
      } catch (err) {
        setFieldErrors((prev) => ({
          ...prev,
          [fieldKey]: extractErrorMessage(err, t('shared.ui.settings.environment.saveError', 'Failed to save configuration')),
        }));
      }
    },
    [canSave, createMutation, currentConfig, draftValues, schemaFields, updateMutation],
  );

  const handleRestore = useCallback(
    async (fieldKey: string) => {
      const field = schemaFields.find((f) => f.key === fieldKey);
      if (!canSave || !field || !currentConfig?.id || field.defaultValue === undefined) return;

      setDraftValues((prev) => ({ ...prev, [fieldKey]: String(field.defaultValue) }));

      try {
        await updateMutation.mutateAsync({
          configId: String(currentConfig.id),
          body: {
            label: currentConfig.label,
            shared: true,
            data: { ...currentConfig.data, [fieldKey]: field.defaultValue },
          },
        });
        clearFieldError(setFieldErrors, fieldKey);
      } catch (err) {
        setFieldErrors((prev) => ({
          ...prev,
          [fieldKey]: extractErrorMessage(err, t('shared.ui.settings.environment.restoreError', 'Failed to restore configuration')),
        }));
      }
    },
    [canSave, currentConfig, schemaFields, updateMutation],
  );

  // Don't render outside the public project
  if (!isPublic) return null;

  return (
    <Box sx={styles.content}>
      {isError && (
        <Typography
          role="alert"
          sx={styles.loadError}
        >
          {t('shared.ui.settings.environment.loadError', 'The environment settings did not load, so the current values are unknown. Reload the page before you edit them.')}
        </Typography>
      )}
      {schemaFields.map((field) => (
        <EnvironmentFieldRow
          key={field.key}
          field={field}
          value={draftValues[field.key] ?? ''}
          disabled={!canEdit || isBusy || isError}
          error={fieldErrors[field.key]}
          onChange={handleChange}
          onBlur={(...args) => void handleBlur(...args)}
          onRestore={(...args) => void handleRestore(...args)}
        />
      ))}
    </Box>
  );
});

/* ── styles ───────────────────────────────────────────────────────────── */

const styles: Record<string, SxProps<Theme>> = {
  content: (theme) => ({
    display: 'flex',
    flexDirection: 'column',
    alignItems: 'center',
    padding: `${theme.spacing(2.75)}rem`,
    gap: theme.spacing(3),
    backgroundColor: theme.vars.palette.background.tabPanel,
    height: '100%',
    overflowY: 'auto',
  }),
  loadError: (theme) => ({
    color: theme.vars.palette.error.main,
    alignSelf: 'stretch',
  }),
};
