/**
 * Query keys. Everything that belongs to one project lives under
 * `['p', projectId, …]`, so switching projects drops it with a single
 * `removeQueries({ queryKey: projectRoot(old) })`.
 */
export const queryKeys = {
  projects: () => ['projects'] as const,
  projectRoot: (projectId: number) => ['p', projectId] as const,
  conversations: (projectId: number) => ['p', projectId, 'conversations'] as const,
  conversation: (projectId: number, conversationId: string) =>
    ['p', projectId, 'conversation', conversationId] as const,
  agents: (projectId: number) => ['p', projectId, 'agents'] as const,
  models: (projectId: number) => ['p', projectId, 'models'] as const,
  notifications: (projectId: number) => ['p', projectId, 'notifications'] as const,
  /** Persisted tool calls of a project's conversations (listMessageTraces), keyed below by conversation and groups. */
  traces: (projectId: number) => ['p', projectId, 'traces'] as const,
};
