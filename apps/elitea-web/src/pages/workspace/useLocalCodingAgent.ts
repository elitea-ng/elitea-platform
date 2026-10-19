/**
 * "Create a Local coding agent": what a session offers when the bound
 * project has no classic agent (issue #1218, stage 3).
 *
 * The agent is an ordinary one, created only on the person's click, in the
 * bound project, through the public create-application API under their own
 * permissions: the reviewed instructions shipped with the desktop entry
 * (`entries/desktop/agents/local-coding-agent.md`), the project's default
 * chat model and `meta.step_limit: 100`. No tools are attached: a local turn
 * gets its file and shell tools from the desktop host. Once created it is
 * edited, versioned and governed on the web like any other agent.
 */
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useMemo } from 'react';

import instructions from '@/entries/desktop/agents/local-coding-agent.md?raw';
import { useListModelsQuery, type ConfigModel } from '@/shared/api/configurationsApi';
import { createApplication, getListApplicationsQueryKey } from '@/shared/api/generated/applications/applications';
import { usePermissionList } from '@/shared/api/generated/auth/auth';
import type { ApplicationCreateRequest, Permission } from '@/shared/api/generated/model';
import { PERMISSIONS } from '@/shared/lib/permissions';

const LOCAL_CODING_AGENT_NAME = 'Local coding agent';
/** The agent's own step limit: a workspace turn's default, set explicitly so the platform's stamp does not lower it. */
const LOCAL_CODING_AGENT_STEPS = 100;

/**
 * `checking` while the permissions and models load; `ready` to create;
 * `forbidden` when the person may not create agents in the project;
 * `no_model` when the project has no chat model to give it.
 */
type LocalCodingAgentState = 'checking' | 'ready' | 'forbidden' | 'no_model';

export interface LocalCodingAgent {
  state: LocalCodingAgentState;
  creating: boolean;
  /** The last create's failure, written for a person. */
  error: string | undefined;
  create: () => void;
}

/** The create-application body for `model` (the project's default chat model). */
function localCodingAgentBody(model: Pick<ConfigModel, 'name' | 'project_id'>): ApplicationCreateRequest {
  const modelProject = Number(model.project_id);
  return {
    name: LOCAL_CODING_AGENT_NAME,
    description: 'Explores, edits and tests code in a folder on your computer, through the Elitea desktop app.',
    type: 'interface',
    versions: [
      {
        agent_type: 'openai',
        instructions,
        llm_settings: { model_name: model.name, ...(Number.isInteger(modelProject) ? { model_project_id: modelProject } : {}) },
        meta: { step_limit: LOCAL_CODING_AGENT_STEPS },
      },
    ],
  };
}

/** The model the platform names as the default, else the one flagged default, else the first (as the agent editor picks it). */
function defaultModel(items: readonly ConfigModel[], defaultName: string): ConfigModel | undefined {
  return items.find((model) => defaultName !== '' && model.name === defaultName) ?? items.find((model) => model.default === true) ?? items[0];
}

type QueryState = 'pending' | 'error' | 'success';

function queryState(query: { isSuccess: boolean; isError: boolean }): QueryState {
  if (query.isSuccess) return 'success';
  return query.isError ? 'error' : 'pending';
}

/** Unreadable permissions count as none: the offer is never made on a guess. */
function offerState(input: { permissions: QueryState; mayCreate: boolean; models: QueryState; hasModel: boolean }): LocalCodingAgentState {
  if (input.permissions === 'error' || (input.permissions === 'success' && !input.mayCreate)) return 'forbidden';
  if (input.permissions === 'pending' || input.models === 'pending') return 'checking';
  return input.models === 'success' && input.hasModel ? 'ready' : 'no_model';
}

/** The offer for `projectId`; queries run only while `wanted` (the project has no agent). */
export function useLocalCodingAgent(projectId: number, wanted: boolean): LocalCodingAgent {
  const project = String(projectId);
  const queryClient = useQueryClient();
  const permissions = usePermissionList(project, { query: { enabled: wanted } });
  const models = useListModelsQuery({ projectId: project, include_shared: true }, { enabled: wanted });
  const mayCreate = useMemo(() => {
    // `eliteaFetch` throws on an error status, so `data` is the list (the cast convention of `useHasPermission`).
    const list = permissions.data?.data as Permission[] | undefined;
    return list?.some((entry) => entry.enabled && entry.name === PERMISSIONS.applications.create) ?? false;
  }, [permissions.data]);
  const model = defaultModel(models.data?.items ?? [], models.data?.default_model_name ?? '');

  const mutation = useMutation({
    mutationFn: async () => {
      if (model === undefined) throw new Error('This project has no chat model.');
      await createApplication(project, localCodingAgentBody(model));
    },
    // The session's agent list reads again and picks the new agent.
    onSuccess: () => queryClient.invalidateQueries({ queryKey: getListApplicationsQueryKey(project) }),
  });

  return {
    state: offerState({ permissions: queryState(permissions), mayCreate, models: queryState(models), hasModel: model !== undefined }),
    creating: mutation.isPending,
    error: mutation.error === null ? undefined : mutation.error.message,
    create: () => mutation.mutate(),
  };
}
