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
  /** The caller's like/dislike per answer (client contract 1.3), keyed below by message uuid. */
  feedback: (projectId: number) => ['p', projectId, 'feedback'] as const,
  /** Recommended agents for the empty new-chat screen (client contract 1.3). */
  recommendations: (projectId: number) => ['p', projectId, 'recommendations'] as const,
  /** The caller's own memories (client contract 1.3); online only, never in the offline store. */
  memories: (projectId: number) => ['p', projectId, 'memories'] as const,
  /** Current-period usage against the budget (client contract 1.3), keyed below by scope. */
  usage: (projectId: number) => ['p', projectId, 'usage'] as const,
};
