/**
 * The toolkit pages' half of the connector catalogue (ADR-0044 decision 1,
 * #1501): a save refused with 403 `connector_not_approved` names the
 * connector, and the page offers "Request connector" for it instead of a
 * bare failure line. `wrap` passes the save through untouched and records
 * the refusal on the way; `notice` is what the page renders.
 */
import { useCallback, useState, type ReactNode } from 'react';

import { ConnectorNotApprovedNotice, readConnectorRefusal, type ConnectorTarget } from '@/features/connector-requests';

export interface ConnectorRefusal<Args, Result> {
  readonly wrapped: (args: Args) => Promise<Result>;
  /** The request offer for the last refused save, else `fallback` (the page's own failure line). */
  readonly orFallback: (fallback: ReactNode) => ReactNode;
}

export function useConnectorRefusal<Args, Result>(
  save: (args: Args) => Promise<Result>,
  projectId: string | undefined,
): ConnectorRefusal<Args, Result> {
  const [target, setTarget] = useState<ConnectorTarget>();
  const wrapped = useCallback(
    async (args: Args) => {
      setTarget(undefined);
      try {
        return await save(args);
      } catch (error) {
        setTarget(readConnectorRefusal(error));
        throw error;
      }
    },
    [save],
  );
  const orFallback = (fallback: ReactNode): ReactNode =>
    target === undefined ? fallback : <ConnectorNotApprovedNotice key={target.target} projectId={projectId} target={target} />;
  return { wrapped, orFallback };
}
