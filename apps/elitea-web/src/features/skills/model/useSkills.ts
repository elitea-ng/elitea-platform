import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import {
  createSkill,
  createSkillVersion,
  deleteSkill,
  deleteSkillVersion,
  fetchSkill,
  fetchSkills,
  generateSkillDraft,
  importSkill,
  restoreSkillVersion,
  setDefaultSkillVersion,
  updateSkill,
} from '../api/skillsApi';

export const skillQueryKeys = {
  all: (projectId: string) => ['skills', projectId] as const,
  list: (projectId: string, query: string) => ['skills', projectId, 'list', query] as const,
  detail: (projectId: string, skillId: string, versionId?: string) =>
    ['skills', projectId, 'detail', skillId, versionId ?? 'default'] as const,
};

interface UpdateSkillMutationArgs {
  readonly skillId: string;
  readonly input: Parameters<typeof updateSkill>[2];
  readonly versionId?: string;
}

export function useSkills(projectId: string | undefined, query: string) {
  return useQuery({
    queryKey: skillQueryKeys.list(projectId ?? '', query),
    queryFn: () => fetchSkills(projectId ?? '', { query, sortBy: 'created_at', sortOrder: 'desc' }),
    enabled: projectId !== undefined,
  });
}

export function useSkill(projectId: string | undefined, skillId: string | undefined, versionId?: string) {
  return useQuery({
    queryKey: skillQueryKeys.detail(projectId ?? '', skillId ?? '', versionId),
    queryFn: () => fetchSkill(projectId ?? '', skillId ?? '', versionId),
    enabled: projectId !== undefined && skillId !== undefined,
  });
}

export function useSkillMutations(projectId: string | undefined) {
  const queryClient = useQueryClient();
  const invalidate = async (): Promise<void> => {
    if (projectId) await queryClient.invalidateQueries({ queryKey: skillQueryKeys.all(projectId) });
  };

  return {
    create: useMutation({
      mutationFn: (input: Parameters<typeof createSkill>[1]) => createSkill(projectId ?? '', input),
      onSuccess: invalidate,
    }),
    update: useMutation({
      mutationFn: (args: UpdateSkillMutationArgs) =>
        updateSkill(projectId ?? '', args.skillId, args.input, args.versionId),
      onSuccess: invalidate,
    }),
    createVersion: useMutation({
      mutationFn: (args: {
        readonly skillId: string;
        readonly input: Parameters<typeof createSkillVersion>[2];
      }) => createSkillVersion(projectId ?? '', args.skillId, args.input),
      onSuccess: invalidate,
    }),
    remove: useMutation({
      mutationFn: (args: { readonly skillId: string; readonly versionId?: string }) =>
        deleteSkill(projectId ?? '', args.skillId, args.versionId),
      onSuccess: invalidate,
    }),
    setDefault: useMutation({
      mutationFn: (args: { readonly skillId: string; readonly versionId: string | number }) =>
        setDefaultSkillVersion(projectId ?? '', args.skillId, args.versionId),
      onSuccess: invalidate,
    }),
    restoreVersion: useMutation({
      mutationFn: (args: { readonly skillId: string; readonly versionId: string | number }) =>
        restoreSkillVersion(projectId ?? '', args.skillId, args.versionId),
      onSuccess: invalidate,
    }),
    deleteVersion: useMutation({
      mutationFn: (args: { readonly skillId: string; readonly versionId: string }) =>
        deleteSkillVersion(projectId ?? '', args.skillId, args.versionId),
      onSuccess: invalidate,
    }),
    generate: useMutation({
      mutationFn: (description: string) => generateSkillDraft(projectId ?? '', description),
    }),
    importFile: useMutation({
      mutationFn: (file: File) => importSkill(projectId ?? '', file),
      onSuccess: invalidate,
    }),
  };
}
