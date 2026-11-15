/**
 * The skill publish/unpublish state machine, and the two gates in front of it.
 *
 * Ported from the reference's `usePublishSkill.hooks.js` +
 * `useUnpublishSkillMenu.hooks.jsx` (frontends/EliteaUI), against this app's
 * stack: TanStack Query instead of RTK Query, no Formik context (the editor
 * passes the skill in), and no AI-retry loop — the retry there exists to give a
 * flaky LLM validator a second chance, and this service's gate is
 * deterministic, so a retry would produce the identical answer.
 *
 * ## Two gates, and why both are the SERVER's answer
 *
 *  1. **Permission.** `models.applications.skills.publish`, the same string the
 *     publish routes are mounted behind (`internal/api/router.go`). Read from
 *     the caller's own permission list, never inferred from a role name.
 *  2. **Platform policy.** `is_skill_publish_blocked` and its whitelist, read
 *     from `platform_settings` — the same rows `publish_skill` enforces. A UI
 *     that guessed would either hide a control that works or offer one that
 *     answers 403; publishing the pair is what lets this say WHY it is off.
 *
 * The public project is exempt from the policy gate, because an admin publish
 * from it publishes in place and the server exempts it too.
 *
 * Nothing here is a substitute for the server's enforcement — a hidden button
 * is presentation, and the route refuses on its own.
 */
import { useCallback, useMemo, useState } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import { useGetPlatformSettings } from '@/shared/api/generated/admin/admin';
import { isPublicProject } from '@/entities/project';
import { getConfig } from '@/shared/config';
import { PERMISSIONS } from '@/shared/lib/permissions';

import {
  fetchSkillCategories,
  publishErrorMessage,
  publishSkill,
  unpublishSkill,
  validateSkillForPublish,
} from '../api/skillPublishApi';
import type { SkillCategory, SkillValidationReport } from './publishTypes';
import { skillQueryKeys } from './useSkills';

/** Where the publish dialog is in its three steps. */
type PublishStep = 'preparation' | 'validation' | 'publishing';

const skillPublishQueryKeys = {
  categories: (projectId: string) => ['skills', projectId, 'categories'] as const,
};

export function useSkillCategories(projectId: string | undefined) {
  return useQuery({
    queryKey: skillPublishQueryKeys.categories(projectId ?? ''),
    queryFn: () => fetchSkillCategories(projectId ?? ''),
    enabled: projectId !== undefined,
  });
}

/**
 * Resolves the platform's skill-publishing policy for THIS project.
 *
 * An empty whitelist while blocked means nobody may publish — not "no
 * restrictions". Reading it the other way would invert the control at exactly
 * the moment an operator first switches it on and has not yet added an
 * exemption, which is the one moment they are watching it.
 */
function useSkillPublishPolicy(projectId: string | undefined): {
  readonly blocked: boolean;
  readonly isAdminPublish: boolean;
} {
  // The generated hook takes no project id: `platform_settings` is mounted
  // without one (`/elitea_core/platform_settings/prompt_lib`), and the two
  // guardrail pairs it carries are platform-wide rows rather than per-project
  // ones. The project id is still needed HERE, to test the whitelist against.
  const settings = useGetPlatformSettings();
  // `entities/project`'s selector + `shared/config`, not
  // `src/routes/-guards/publicProject.ts`: that module has the same two-line
  // body but lives in a layer `features/` must not depend on (routes compose
  // features, not the other way around) — the same call
  // `pages/agents/lib/isPublicAgentsProject.ts` makes for the same reason.
  const config = getConfig();
  const isAdminPublish =
    projectId !== undefined &&
    config.status === 'ok' &&
    isPublicProject(projectId, config.config.vite_public_project_id);

  return useMemo(() => {
    const values = settings.data?.data as
      | {
          readonly is_skill_publish_blocked?: boolean;
          readonly skill_publish_whitelist_project_ids?: readonly number[];
        }
      | undefined;
    if (isAdminPublish || values?.is_skill_publish_blocked !== true) {
      return { blocked: false, isAdminPublish };
    }
    const whitelist = values.skill_publish_whitelist_project_ids ?? [];
    return { blocked: !whitelist.includes(Number(projectId)), isAdminPublish };
  }, [settings.data, projectId, isAdminPublish]);
}

export interface SkillPublishTarget {
  readonly skillId: number | undefined;
  readonly versionId: number | undefined;
  /** Status of the version in view: only a `draft` can be published. */
  readonly versionStatus: string | undefined;
  /** Every version name on this skill, for the collision check below. */
  readonly versionNames: readonly string[];
}

export interface SkillPublishingState {
  /** The control renders at all: the caller holds the publish permission. */
  readonly canShowPublish: boolean;
  /** …and the platform is not refusing publishes from this project. */
  readonly canPublish: boolean;
  readonly blockedByPolicy: boolean;
  readonly canUnpublish: boolean;
  readonly isOpen: boolean;
  readonly step: PublishStep;
  readonly versionName: string;
  readonly versionNameError: string | undefined;
  readonly category: string;
  readonly releaseNotes: string;
  readonly categories: readonly SkillCategory[];
  readonly report: SkillValidationReport | undefined;
  readonly error: string | undefined;
  readonly isValidating: boolean;
  readonly isPublishing: boolean;
  readonly isUnpublishing: boolean;
  readonly open: () => void;
  readonly close: () => void;
  readonly setVersionName: (value: string) => void;
  readonly setCategory: (value: string) => void;
  readonly setReleaseNotes: (value: string) => void;
  /** Clears a refusal that was surfaced outside the dialog (unpublish). */
  readonly dismissError: () => void;
  readonly validate: () => Promise<void>;
  readonly publish: () => Promise<void>;
  readonly unpublish: () => Promise<void>;
}

const VERSION_NAME_PATTERN = /^[a-zA-Z0-9._-]{1,50}$/;

export function useSkillPublishing(
  projectId: string | undefined,
  target: SkillPublishTarget,
  permissions: ReadonlySet<string>,
  onPublished?: () => void,
): SkillPublishingState {
  const queryClient = useQueryClient();
  const policy = useSkillPublishPolicy(projectId);
  const categories = useSkillCategories(projectId);

  const [isOpen, setIsOpen] = useState(false);
  const [step, setStep] = useState<PublishStep>('preparation');
  const [versionName, setVersionNameState] = useState('');
  const [category, setCategoryState] = useState('');
  // Not part of the validation verdict: notes do not change what the gate checks.
  const [releaseNotes, setReleaseNotes] = useState('');
  const [report, setReport] = useState<SkillValidationReport>();
  const [token, setToken] = useState<string>();
  const [error, setError] = useState<string>();

  const canShowPublish = permissions.has(PERMISSIONS.skills.publish) && target.versionStatus === 'draft';
  const canUnpublish = permissions.has(PERMISSIONS.skills.publish) && target.versionStatus === 'published';

  const invalidate = useCallback(async (): Promise<void> => {
    if (projectId) await queryClient.invalidateQueries({ queryKey: skillQueryKeys.all(projectId) });
  }, [projectId, queryClient]);

  const validateMutation = useMutation({
    mutationFn: () =>
      validateSkillForPublish(projectId ?? '', target.skillId ?? 0, target.versionId ?? 0, {
        versionName: versionName.trim(),
        category,
      }),
  });

  const publishMutation = useMutation({
    mutationFn: () =>
      publishSkill(projectId ?? '', target.skillId ?? 0, target.versionId ?? 0, {
        versionName: versionName.trim(),
        category,
        ...(releaseNotes.trim() ? { releaseNotes: releaseNotes.trim() } : {}),
        ...(token ? { validationToken: token } : {}),
      }),
    onSuccess: invalidate,
  });

  const unpublishMutation = useMutation({
    mutationFn: () => unpublishSkill(projectId ?? '', target.skillId ?? 0, target.versionId ?? 0),
    onSuccess: invalidate,
  });

  /**
   * The name check runs HERE as well as on the server, and the duplicate is
   * deliberate: the server's refusal costs a round trip and arrives after the
   * user has moved on, while this one is the same rule (`^[a-zA-Z0-9._-]{1,50}$`
   * plus "not already used on this skill") shown as they type. The server stays
   * the authority — a name that slips past this is still refused there.
   */
  const versionNameError = useMemo(() => {
    const trimmed = versionName.trim();
    if (!trimmed) return undefined;
    if (!VERSION_NAME_PATTERN.test(trimmed)) {
      return 'Use up to 50 letters, digits, dots, dashes or underscores.';
    }
    return target.versionNames.includes(trimmed)
      ? 'A version with this name already exists. Choose a different name.'
      : undefined;
  }, [versionName, target.versionNames]);

  /**
   * Editing either input DISCARDS the gate's verdict.
   *
   * Without this a FAIL is a dead end: the dialog swaps Continue for a Publish
   * button that `report.status === 'FAIL'` keeps disabled forever, so a user who
   * fixes the very thing the gate complained about — a colliding version name —
   * has no control left that re-runs it, and has to cancel and start over. The
   * verdict is about THIS name and THIS category, so it stops being an answer
   * the moment either changes.
   *
   * The token goes with it. It is the server's receipt for content it approved,
   * and publishing on a receipt that no longer describes the request is exactly
   * what the server re-validates against.
   */
  const discardVerdict = useCallback((): void => {
    setReport(undefined);
    setToken(undefined);
    setError(undefined);
    setStep('preparation');
  }, []);

  const setVersionName = useCallback(
    (value: string): void => {
      setVersionNameState(value);
      discardVerdict();
    },
    [discardVerdict],
  );

  const setCategory = useCallback(
    (value: string): void => {
      setCategoryState(value);
      discardVerdict();
    },
    [discardVerdict],
  );

  const dismissError = useCallback((): void => setError(undefined), []);

  const reset = useCallback((): void => {
    setStep('preparation');
    setVersionNameState('');
    setCategoryState('');
    setReleaseNotes('');
    setReport(undefined);
    setToken(undefined);
    setError(undefined);
  }, []);

  const open = useCallback((): void => {
    reset();
    setIsOpen(true);
  }, [reset]);

  const close = useCallback((): void => {
    setIsOpen(false);
    reset();
  }, [reset]);

  const validate = useCallback(async (): Promise<void> => {
    setError(undefined);
    setStep('validation');
    try {
      const result = await validateMutation.mutateAsync();
      setReport(result);
      // A FAIL carries no token, and publishing without one re-runs the gate
      // server-side — so clearing it here cannot be used to skip validation.
      setToken(result.validation_token);
    } catch (caught) {
      setError(publishErrorMessage(caught, 'Validation failed.'));
      setStep('preparation');
    }
  }, [validateMutation]);

  const publish = useCallback(async (): Promise<void> => {
    setError(undefined);
    setStep('publishing');
    try {
      await publishMutation.mutateAsync();
      onPublished?.();
      close();
    } catch (caught) {
      setError(publishErrorMessage(caught, 'Publishing failed.'));
      setStep('validation');
    }
  }, [publishMutation, onPublished, close]);

  const unpublish = useCallback(async (): Promise<void> => {
    setError(undefined);
    try {
      await unpublishMutation.mutateAsync();
      onPublished?.();
    } catch (caught) {
      setError(publishErrorMessage(caught, 'Unpublishing failed.'));
    }
  }, [unpublishMutation, onPublished]);

  return {
    canShowPublish,
    canPublish: canShowPublish && !policy.blocked,
    blockedByPolicy: policy.blocked,
    canUnpublish,
    isOpen,
    step,
    versionName,
    versionNameError,
    category,
    releaseNotes,
    categories: categories.data ?? [],
    report,
    error,
    isValidating: validateMutation.isPending,
    isPublishing: publishMutation.isPending,
    isUnpublishing: unpublishMutation.isPending,
    open,
    close,
    setVersionName,
    setCategory,
    setReleaseNotes,
    dismissError,
    validate,
    publish,
    unpublish,
  };
}
