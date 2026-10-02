import { vi } from 'vitest';

vi.mock('@/shared/api/configurationsApi', () => ({
  useListModelsQuery: () => ({ data: { items: [
    { id: 'default', name: 'default-model', project_id: '9' },
    { id: 'shared', name: 'pipeline-model', project_id: '1', display_name: 'Shared model' },
    { id: 'private', name: 'pipeline-model', project_id: '9', display_name: 'Private model' },
  ] } }),
}));
