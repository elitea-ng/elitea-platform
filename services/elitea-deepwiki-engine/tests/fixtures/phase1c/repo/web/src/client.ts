export type UserPublic = {
  id: number;
  full_name?: string | null;
  is_active: boolean;
};

export async function readMe(): Promise<UserPublic> {
  const response = await fetch('/api/v1/users/me');
  return response.json();
}

export function hashUser(userId: number) {
  return axios.post(`/users/${userId}/hash`);
}

export class ItemsClient {
  list(projectId: number) {
    return __request(OpenAPI, { method: 'GET', url: '/api/v1/inventory/items/{var}' });
  }
}
