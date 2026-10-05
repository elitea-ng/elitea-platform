/**
 * Process-wide registry of in-flight work that must stop when the session ends
 * (REST calls and event streams). `endSession` calls `abortAll()`.
 *
 * Work tied to one project (an execution stream, the notifications stream) is
 * registered with a scope such as `projectScope(7)`, so switching projects can
 * stop exactly that work with `abortScope()` and leave the rest running.
 */
const controllers = new Map<AbortController, string | null>();

/** Registers a controller (optionally under `scope`); returns the function that unregisters it. */
export function registerAbortable(controller: AbortController, scope: string | null = null): () => void {
  controllers.set(controller, scope);
  return () => {
    controllers.delete(controller);
  };
}

/** Aborts and forgets every registered controller. Returns how many were aborted. */
export function abortAll(reason: string = 'session_ended'): number {
  const pending = [...controllers.keys()];
  controllers.clear();
  for (const controller of pending) {
    controller.abort(new Error(reason));
  }
  return pending.length;
}

/** Aborts and forgets the controllers registered under `scope`. Returns how many were aborted. */
export function abortScope(scope: string, reason: string = 'scope_ended'): number {
  const pending = [...controllers].filter(([, s]) => s === scope).map(([c]) => c);
  for (const controller of pending) {
    controllers.delete(controller);
    controller.abort(new Error(reason));
  }
  return pending.length;
}

/** The abort scope of work that belongs to one project. */
export function projectScope(projectId: number): string {
  return `project:${projectId}`;
}

export function inFlightCount(): number {
  return controllers.size;
}
