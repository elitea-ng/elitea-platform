import { createContext, useContext, useMemo, useState, type ReactNode } from 'react';

/**
 * In-memory workspace (server) selection. Skeleton only.
 *
 * TODO: persist the origin (expo-secure-store, device-only), and on clear() run
 * the full wipe described in docs/design.md (tokens, SQLCipher DB, attachments,
 * outbox) before returning to /connect.
 */
interface WorkspaceState {
  serverOrigin: string | null;
  connect: (origin: string) => void;
  clear: () => void;
}

const WorkspaceContext = createContext<WorkspaceState | null>(null);

export function WorkspaceProvider({ children }: { children: ReactNode }) {
  const [serverOrigin, setServerOrigin] = useState<string | null>(null);
  const value = useMemo<WorkspaceState>(
    () => ({
      serverOrigin,
      connect: setServerOrigin,
      clear: () => setServerOrigin(null),
    }),
    [serverOrigin],
  );
  return <WorkspaceContext.Provider value={value}>{children}</WorkspaceContext.Provider>;
}

export function useWorkspace(): WorkspaceState {
  const ctx = useContext(WorkspaceContext);
  if (!ctx) throw new Error('useWorkspace must be used inside <WorkspaceProvider>');
  return ctx;
}
