import { useState } from 'react';

import { useNavigate } from '@tanstack/react-router';

import { skillVersionKey, useSkillMutations, type SkillRecord } from '@/features/skills';
import { t } from '@/shared/i18n';

/**
 * The skill editor's version-scoped controls (#874): compare, restore
 * (rollback) and delete-version — split out of `EditSkill.tsx` for the same
 * reason `pages/agents/lib/useEditApplicationVersionControls.ts` exists: the
 * page component hit the §3.5 complexity budget (18 against 12) and the
 * §3.4 file-length budget (482 lines against 400) once this state and these
 * three handlers were inline.
 *
 * Mirrors that agents hook's shape: the page still owns navigation and
 * decides what happens after a mutation succeeds, this hook owns dialog
 * open/close state and the mutation calls themselves.
 */
export function useEditSkillVersionControls(args: {
  readonly tab: string | undefined;
  readonly skillId: string | undefined;
  readonly version: string | undefined;
  readonly skill: SkillRecord | undefined;
  readonly mutations: ReturnType<typeof useSkillMutations>;
  readonly setError: (message: string) => void;
}) {
  const { tab, skillId, version, skill, mutations, setError } = args;
  const navigate = useNavigate();

  const [compareOpen, setCompareOpen] = useState(false);
  const [restoreOpen, setRestoreOpen] = useState(false);
  const [deleteVersionOpen, setDeleteVersionOpen] = useState(false);

  const versions = skill?.versions ?? [];
  const baseVersion = versions.find((v) => v.name === 'base');
  const activeVersionKey =
    version ?? (skill?.version_details ? skillVersionKey(skill.version_details) : undefined) ?? '';
  const activeVersionName = versions.find((v) => skillVersionKey(v) === activeVersionKey)?.name;

  const goToVersion = (targetVersion: string): void => {
    void navigate({
      to: '/skills/$tab/$skillId/$version',
      params: { tab: tab ?? 'all', skillId: skillId ?? '', version: targetVersion },
    });
  };

  const goToBase = (): void => {
    if (baseVersion) goToVersion(skillVersionKey(baseVersion));
  };

  return {
    versions,
    baseVersion,
    activeVersionKey,
    /** "base" is reserved — see SkillVersion's Go doc comment — so it is never a valid target for restore/delete. */
    isNamedVersion: activeVersionName !== undefined && activeVersionName !== 'base',
    goToVersion,
    compare: {
      open: compareOpen,
      onOpen: () => setCompareOpen(true),
      onClose: () => setCompareOpen(false),
    },
    restore: {
      open: restoreOpen,
      onOpen: () => setRestoreOpen(true),
      onClose: () => setRestoreOpen(false),
      confirming: mutations.restoreVersion.isPending,
      onConfirm: () => {
        if (!skillId || !version) return;
        void mutations.restoreVersion
          .mutateAsync({ skillId, versionId: version })
          .then(() => {
            setRestoreOpen(false);
            goToBase();
          })
          .catch(() => setError(t('skills.edit.restoreError', 'Failed to restore this version.')));
      },
    },
    deleteVersion: {
      open: deleteVersionOpen,
      name: versions.find((v) => skillVersionKey(v) === version)?.name ?? '',
      onOpen: () => setDeleteVersionOpen(true),
      onClose: () => setDeleteVersionOpen(false),
      confirming: mutations.deleteVersion.isPending,
      onConfirm: () => {
        if (!skillId || !version) return;
        void mutations.deleteVersion
          .mutateAsync({ skillId, versionId: version })
          .then(() => {
            setDeleteVersionOpen(false);
            goToBase();
          })
          .catch(() => setError(t('skills.edit.deleteVersionError', 'Failed to delete this version.')));
      },
    },
  };
}
