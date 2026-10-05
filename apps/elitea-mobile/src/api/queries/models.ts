import { useQuery } from '@tanstack/react-query';
import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { unwrap } from '@/api/errors';
import type { components } from '@/api/generated/elitea.gen';
import { queryKeys } from '@/api/queries/keys';

/**
 * The chat models a project can use (`listConfigurationModels`), and the
 * create call for a plain model conversation: no agent, one `dummy`
 * participant carrying the model, as the web app creates it. Turns in it use
 * the adhoc contract (`sendMessage.ts`).
 */

export interface ChatModel {
  name: string;
  displayName: string;
  /** The project the model belongs to (another one when it is shared). */
  projectId: number;
  isDefault: boolean;
}

export interface ModelCatalogue {
  models: ChatModel[];
  /** The project default, else the first model; null when there is none. */
  defaultModel: ChatModel | null;
}

const ModelRow = z.looseObject({
  name: z.string().max(500),
  display_name: z.string().max(500).nullish(),
  project_id: z.number().int().positive(),
  default: z.boolean().nullish(),
  // `compat` deployments list every model section; only chat models apply.
  section: z.string().nullish(),
});

const Catalogue = z.looseObject({
  items: z.array(z.unknown()).nullish(),
  default_model_name: z.string().nullish().catch(null),
  default_model_project_id: z.number().nullish().catch(null),
});

export async function fetchModels(
  projectId: number,
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<ModelCatalogue> {
  const raw = await unwrap(
    client.GET('/configurations/models/{project_id}', {
      params: { path: { project_id: String(projectId) }, query: { section: 'llm', include_shared: true } },
      signal,
    }),
  );
  const catalogue = Catalogue.safeParse(raw);
  const envelope = catalogue.success ? catalogue.data : { items: [], default_model_name: null, default_model_project_id: null };
  const defaultName = envelope.default_model_name?.trim() || null;
  const defaultProject = envelope.default_model_project_id ?? null;
  const models: ChatModel[] = [];
  const seen = new Set<string>();
  for (const item of envelope.items ?? []) {
    const row = ModelRow.safeParse(item);
    if (!row.success) continue;
    const name = row.data.name.trim();
    if (name === '' || (row.data.section && row.data.section !== 'llm_model')) continue;
    const key = `${row.data.project_id}:${name}`;
    if (seen.has(key)) continue;
    seen.add(key);
    const isDefault =
      row.data.default === true ||
      (defaultName === name && (defaultProject === null || defaultProject === row.data.project_id));
    models.push({ name, displayName: row.data.display_name?.trim() || name, projectId: row.data.project_id, isDefault });
  }
  // The default first, then the server's order.
  models.sort((a, b) => Number(b.isDefault) - Number(a.isDefault));
  return { models, defaultModel: models[0] ?? null };
}

export function useModels(projectId: number | null, enabled: boolean = true) {
  return useQuery({
    queryKey: queryKeys.models(projectId ?? 0),
    enabled: enabled && projectId !== null,
    queryFn: ({ signal }) => fetchModels(projectId as number, sessionApiClient(), signal),
  });
}

type CreateConversationRequest = components['schemas']['CreateConversationRequest'];

const Created = z.looseObject({
  id: z
    .union([z.number(), z.string().regex(/^\d{1,15}$/)])
    .transform(Number)
    .refine((n) => Number.isSafeInteger(n) && n > 0),
  uuid: z.string().nullish(),
});

export class StartModelConversationError extends Error {
  constructor() {
    super('could not start a model conversation: bad_response');
    this.name = 'StartModelConversationError';
  }
}

/** Creates a private conversation with `model`; returns its id (route reference). */
export async function startConversationWithModel(
  projectId: number,
  model: ChatModel,
  client: ApiClient = sessionApiClient(),
): Promise<{ id: number; uuid: string | null }> {
  const body: CreateConversationRequest = {
    name: model.displayName.slice(0, 200),
    is_private: true,
    participants: [
      {
        entity_name: 'dummy',
        entity_meta: {},
        entity_settings: {
          llm_settings: {
            model_name: model.name,
            // Always named: a model shared from another project resolves only with it.
            model_project_id: model.projectId,
            stream: true,
          },
        },
      },
    ],
  };
  const created = await unwrap(
    client.POST('/elitea_core/conversations/prompt_lib/{project_id}', {
      params: { path: { project_id: String(projectId) } },
      body,
    }),
  );
  const parsed = Created.safeParse(created);
  if (!parsed.success) throw new StartModelConversationError();
  return { id: parsed.data.id, uuid: parsed.data.uuid ?? null };
}
