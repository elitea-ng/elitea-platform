/**
 * Service prompts section — displays prompt cards, supports create/edit/restore
 * via a modal dialog.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/ui/system-prompts/
 * ServicePromptsSection.jsx`.
 *
 * Deviations from the baseline:
 *  - Uses `useSelectedProjectStore` instead of Redux hooks
 *  - Uses the handwritten `configurationsApi` instead of RTK Query slices
 *  - Uses `CodeMirrorEditor` without extensions (markdown support not
 *    installed in this app; plain text editing only)
 *  - No `useToast` — errors surface as inline feedback (toast integration
 *    is a future unit)
 *  - Uses `ExpandedViewerModal` for the create/edit dialog
 *  - No tour IDs
 *  - Key validation is client-side only (matches baseline behavior)
 */
import { memo, useCallback, useMemo, useRef, useState } from 'react';

import type { ConfigurationItem } from '@/shared/api/configurationsApi';
import {
  useCreateConfigurationMutation,
  useGetAvailableConfigurationsTypeQuery,
  useGetConfigurationsListQuery,
  useUpdateConfigurationMutation,
} from '@/shared/api/configurationsApi';
import { usePermissionList } from '@/shared/api/generated/auth/auth';
import type { Permission } from '@/shared/api/generated/model';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { useIsPublicProject } from '@/shared/lib/hooks/usePublicProjectId';
import { useSelectedProjectStore } from '@/widgets/app-shell';
import { t } from '@/shared/i18n';
import type { PromptConfig } from '@/features/settings';
import { servicePromptsFeature } from '@/features/settings';

const { ServicePromptsBody } = servicePromptsFeature;

/* ── helpers ──────────────────────────────────────────────────────────── */

function deriveLabelFromKey(key: string): string {
  const safe = String(key || '').trim();
  if (!safe) return t('shared.ui.settings.prompts.defaultLabel', 'Service prompt');
  return safe
    .split(/[_-]+/)
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join(' ');
}

/** Extract a human-readable message from a thrown error (`EliteaApiError` or otherwise). */
function extractErrorMessage(err: unknown, fallback: string): string {
  if (err instanceof Error && err.message) return err.message;
  return fallback;
}

/* ── component ────────────────────────────────────────────────────────── */
export const ServicePrompts = memo(function ServicePrompts() {
  const projectId = useSelectedProjectStore((s) => s.project?.id ?? '');
  /*
   * The public-project test used to be `projectId === '1'`, a literal, and this
   * whole section is gated on it. On a deployment whose public project is not
   * id 1, every query below stayed disabled and the page rendered its empty
   * state — with no request made, no error, and nothing in the console.
   *
   * `useIsPublicProject` asks the server for the id (published on
   * `platform_settings`) and falls back to the image's
   * `VITE_PUBLIC_PROJECT_ID`. On a deployment whose public project IS id 1 —
   * every reference deployment in this repository — the answer is identical to
   * the literal it replaced.
   */
  const isPublic = useIsPublicProject(projectId);
  const enabled = !!projectId && isPublic;

  /* ── data fetching ─────────────────────────────────────────────────── */
  const { data: configsData, isLoading, isFetching } = useGetConfigurationsListQuery(
    { projectId, section: 'service_prompts', includeShared: false, pageSize: 100 },
    { enabled },
  );

  const { data: availableTypes } = useGetAvailableConfigurationsTypeQuery(
    { section: 'service_prompts' },
    { enabled },
  );

  const permissionsQuery = usePermissionList(projectId, { query: { enabled } });
  const canEdit = useMemo(() => {
    const list = permissionsQuery.data?.data as Permission[] | undefined;
    if (!list) return false;
    return list.some((entry) => entry.enabled && entry.name === PERMISSIONS.configuration.update);
  }, [permissionsQuery.data]);
  /** Combines section-active + permission gates into one flag (keeps handler complexity low). */
  const canManage = useMemo(() => enabled && canEdit, [enabled, canEdit]);

  const createMutation = useCreateConfigurationMutation(projectId);
  const updateMutation = useUpdateConfigurationMutation(projectId);
  const isBusy = isLoading || isFetching || createMutation.isPending || updateMutation.isPending;

  /* ── derived data ──────────────────────────────────────────────────── */
  const prompts = useMemo<PromptConfig[]>(() => {
    const items: ConfigurationItem[] = configsData?.items ?? [];
    return items
      .filter((item) => item.section === 'service_prompts')
      .map((item) => {
        const key = (item.data?.key as string | undefined) ?? item.elitea_title ?? '';
        const prompt = (item.data?.prompt as string | undefined) ?? '';
        return {
          id: item.id,
          key: String(key),
          label: item.label ?? deriveLabelFromKey(String(key)),
          prompt: String(prompt),
        };
      })
      .sort((a, b) => a.key.localeCompare(b.key));
  }, [configsData?.items]);
  const allowedKeys = useMemo(() => {
    const raw = availableTypes?.find((item) => item.type === 'service_prompt')?.config_schema;
    const schema = raw ?? {};
    const data = (schema?.properties as Record<string, unknown>)?.data as Record<string, unknown>;
    const props = (data?.properties as Record<string, unknown>) ?? {};
    const keys = (props?.key as { enum?: unknown[] })?.enum;
    if (Array.isArray(keys) && keys.length) {
      return Array.from(new Set(keys.map((v) => String(v).trim().toLowerCase()).filter(Boolean)));
    }
    return ['mermaid_quick_fix'];
  }, [availableTypes]);

  const usedKeys = useMemo(() => new Set(prompts.map((p) => p.key.toLowerCase())), [prompts]);

  const availableKeys = useMemo(
    () => allowedKeys.filter((key) => !usedKeys.has(key)),
    [allowedKeys, usedKeys],
  );
  const defaultPromptsByKey = useMemo(() => {
    const raw = availableTypes?.find((item) => item.type === 'service_prompt')?.config_schema;
    const schema = raw ?? {};
    const data = (schema?.properties as Record<string, unknown>)?.data as Record<string, unknown>;
    const props = (data?.properties as Record<string, unknown>) ?? {};
    const defaults = (props?.prompt as { default_by_key?: unknown })?.default_by_key;
    if (defaults && typeof defaults === 'object') return defaults;
    return {};
  }, [availableTypes]);

  const getDefaultPrompt = useCallback(
    (key: string): string | null => {
      if (!key) return null;
      const normalized = String(key).toLowerCase();
      const obj = defaultPromptsByKey as Record<string, unknown>;
      return (
        (obj[normalized] as string) ??
        (obj[key] as string) ??
        (obj[key.toLowerCase()] as string) ??
        null
      );
    },
    [defaultPromptsByKey],
  );
  /* ── local state (refs to reduce hook deps) ────────────────────────── */
  const [isOpen, setIsOpen] = useState(false);
  const [selectedConfig, setSelectedConfig] = useState<PromptConfig | null>(null);
  const modeRef = useRef<'create' | 'edit' | null>(null);
  const [draftKey, setDraftKey] = useState('');
  const [draftPrompt, setDraftPrompt] = useState('');
  const [errorMessage, setErrorMessage] = useState<string | null>(null);
  const hasAvailableKeys = availableKeys.length > 0;
  const availableKeysRef = useRef(availableKeys);
  availableKeysRef.current = availableKeys;
  const usedKeysRef = useRef(usedKeys);
  usedKeysRef.current = usedKeys;

  const handleOpenCreate = useCallback(() => {
    if (!canManage) return;
    modeRef.current = 'create';
    setSelectedConfig(null);
    setDraftKey(availableKeysRef.current[0] ?? '');
    setDraftPrompt('');
    setErrorMessage(null);
    setIsOpen(true);
  }, [canManage]);

  const handleOpenEdit = useCallback((config: PromptConfig) => {
    modeRef.current = 'edit';
    setSelectedConfig(config);
    setDraftKey(config.key);
    setDraftPrompt(config.prompt);
    setErrorMessage(null);
    setIsOpen(true);
  }, []);

  const handleDiscard = useCallback(() => {
    setIsOpen(false);
    modeRef.current = null;
    setSelectedConfig(null);
    setDraftKey('');
    setDraftPrompt('');
  }, []);

  /* ── validate key (caches deps) ────────────────────────────────────── */
  const validateKey = useCallback(
    (key: string, { disallowUsed = false } = {}): { ok: boolean; message?: string; key?: string } => {
      const normalized = String(key || '').trim();
      if (!normalized) return { ok: false, message: t('shared.ui.settings.prompts.keyEmpty', 'Key cannot be empty') };
      if (normalized.length > 128) return { ok: false, message: t('shared.ui.settings.prompts.keyTooLong', 'Key must not exceed 128 characters') };
      if (!/^[a-zA-Z0-9_-]+$/.test(normalized)) {
        return { ok: false, message: t('shared.ui.settings.prompts.keyInvalid', 'Key must contain only letters, numbers, underscores, or dashes') };
      }
      const lowered = normalized.toLowerCase();
      if (allowedKeys.length && !allowedKeys.includes(lowered)) {
        return { ok: false, message: t('shared.ui.settings.prompts.keyNotAllowed', 'Key must be selected from the predefined list') };
      }
      if (disallowUsed && usedKeysRef.current.has(lowered)) {
        return { ok: false, message: t('shared.ui.settings.prompts.keyAlreadyUsed', 'This key is already configured') };
      }
      return { ok: true, key: lowered };
    },
    [allowedKeys],
  );

  /* ── save (ref-based deps → ≤ 8) ───────────────────────────────────── */

  /* ── build create body helper ──────────────────────────────────────── */
  const buildCreateBody = useCallback(
    (key: string, promptText: string) => ({
      elitea_title: key,
      label: deriveLabelFromKey(key),
      type: 'service_prompt',
      shared: true,
      data: { key, prompt: promptText },
    }),
    [],
  );

  const buildEditBody = useCallback(
    (config: PromptConfig, key: string, promptText: string) => ({
      label: config.label ?? deriveLabelFromKey(key),
      shared: true,
      data: { key, prompt: promptText },
    }),
    [],
  );

  /** Surfaces a `validateKey` failure message (falls back to a generic message). */
  const reportValidationError = useCallback((message: string | undefined) => {
    setErrorMessage(message ?? t('shared.ui.settings.prompts.keyInvalid', 'Key must contain only letters, numbers, underscores, or dashes'));
  }, []);

  /** Bundled so `handleSave`'s deps stay within the §3.5 hook-deps budget. */
  const saveMutations = useMemo(() => ({ createMutation, updateMutation }), [createMutation, updateMutation]);
  const saveHelpers = useMemo(
    () => ({ buildEditBody, buildCreateBody, reportValidationError }),
    [buildEditBody, buildCreateBody, reportValidationError],
  );

  const handleSave = useCallback(async () => {
    if (!canManage) return;

    const promptText = String(draftPrompt || '').trim();
    if (!promptText) {
      setErrorMessage(t('shared.ui.settings.prompts.promptEmptyError', 'Prompt cannot be empty'));
      return;
    }

    const keyValidation = validateKey(draftKey);
    if (!keyValidation.ok) {
      saveHelpers.reportValidationError(keyValidation.message);
      return;
    }

    const key = keyValidation.key ?? String(draftKey || '').trim();

    try {
      if (modeRef.current === 'edit' && selectedConfig) {
        await saveMutations.updateMutation.mutateAsync({
          configId: String(selectedConfig.id),
          body: saveHelpers.buildEditBody(selectedConfig, key, promptText),
        });
      } else {
        const createKeyValidation = validateKey(draftKey, { disallowUsed: true });
        if (!createKeyValidation.ok) {
          saveHelpers.reportValidationError(createKeyValidation.message);
          return;
        }

        await saveMutations.createMutation.mutateAsync(
          saveHelpers.buildCreateBody(createKeyValidation.key ?? draftKey, promptText),
        );
      }
      setErrorMessage(null);
      handleDiscard();
    } catch (err) {
      setErrorMessage(extractErrorMessage(err, t('shared.ui.settings.prompts.saveError', 'Failed to save prompt')));
    }
  }, [canManage, draftKey, draftPrompt, selectedConfig, validateKey, saveMutations, handleDiscard, saveHelpers]);

  /* ── restore ───────────────────────────────────────────────────────── */
  const handleRestoreToDefault = useCallback(
    async (config: PromptConfig) => {
      if (!canManage || !config.id) return;
      const defaultPromptValue = getDefaultPrompt(config.key);
      if (!defaultPromptValue) {
        setErrorMessage(t('shared.ui.settings.prompts.noDefaultError', 'No default prompt is available for this key'));
        return;
      }

      try {
        await updateMutation.mutateAsync({
          configId: String(config.id),
          body: {
            label: config.label ?? deriveLabelFromKey(config.key),
            shared: true,
            data: { key: config.key, prompt: defaultPromptValue },
          },
        });
        setErrorMessage(null);
      } catch (err) {
        setErrorMessage(extractErrorMessage(err, t('shared.ui.settings.prompts.restoreError', 'Failed to restore prompt')));
      }
    },
    [canManage, getDefaultPrompt, updateMutation],
  );

  const handleRestoreInModal = useCallback(() => {
    if (!selectedConfig) return;
    const defaultPromptValue = getDefaultPrompt(selectedConfig.key);
    if (defaultPromptValue) setDraftPrompt(defaultPromptValue);
  }, [selectedConfig, getDefaultPrompt]);

  const handleDismissError = useCallback(() => setErrorMessage(null), []);

  const hasDefaultPrompt = useCallback((key: string) => Boolean(getDefaultPrompt(key)), [getDefaultPrompt]);

  const hasChanges = useMemo(() => {
    if (modeRef.current === 'create') return draftPrompt.trim().length > 0;
    if (modeRef.current === 'edit' && selectedConfig) {
      return draftKey !== selectedConfig.key || draftPrompt !== selectedConfig.prompt;
    }
    return false;
  }, [draftKey, draftPrompt, selectedConfig]);

  /* ── render ────────────────────────────────────────────────────────── */
  if (!enabled) return null;

  const createTooltip = hasAvailableKeys
    ? t('shared.ui.settings.prompts.createTooltip', 'Create service prompt')
    : t('shared.ui.settings.prompts.allConfiguredTooltip', 'All service prompt keys are already configured');

  const modalTitle = modeRef.current === 'create'
    ? t('shared.ui.settings.prompts.newPromptTitle', 'New Service Prompt')
    : (selectedConfig?.label ?? draftKey ?? t('shared.ui.settings.prompts.editPromptTitle', 'Edit Service Prompt'));

  return (
    <ServicePromptsBody
      header={{ createTooltip, modalTitle }}
      promptData={{ prompts, hasDefaultPrompt }}
      editor={{
        isOpen,
        allowedKeys,
        usedKeysRef,
        modeRef,
        draftKey,
        draftPrompt,
        onDraftKeyChange: setDraftKey,
        onDraftPromptChange: setDraftPrompt,
        onRestoreInModal: handleRestoreInModal,
      }}
      actions={{ handleOpenCreate, handleOpenEdit, handleDiscard, handleSave, handleRestoreToDefault, onDismissError: handleDismissError }}
      flags={{ isBusy, hasAvailableKeys, hasDefault: hasDefaultPrompt(draftKey), hasChanges, canEdit, errorMessage }}
    />
  );
});
