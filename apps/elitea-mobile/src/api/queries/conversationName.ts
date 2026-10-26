import { sessionApiClient, type ApiClient } from '@/api/client';
import { unwrap } from '@/api/errors';

/**
 * A conversation's title from its first exchange (client contract 1.24, `generateConversationName`):
 * the project's default model names it, billed to the caller. The workspace renames the
 * conversation only while its name is still `expectedName`, so a rename made meanwhile stays.
 * Resolves the name the conversation has then. Throws `ApiError`.
 */
export async function generateConversationName(
  projectId: number,
  conversationId: number,
  expectedName: string,
  client: ApiClient = sessionApiClient(),
): Promise<{ name: string; renamed: boolean }> {
  const named = await unwrap(
    client.POST('/elitea_core/conversation_name/prompt_lib/{project_id}/{conversation_id}', {
      params: { path: { project_id: String(projectId), conversation_id: String(conversationId) } },
      body: { expected_name: expectedName },
    }),
  );
  return { name: named?.name ?? expectedName, renamed: named?.renamed === true };
}
