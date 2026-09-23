/**
 * "Every saved credential title this project answers to" — the read half of
 * the attached-toolkit credential warning (#937).
 *
 * `undefined` means NO VERDICT: the read is still in flight, has not been
 * asked for, or failed. `entities/credential`'s `isToolkitCredentialMissing`
 * turns that into "no warning", which is the point — a banner is a claim
 * about the user's data, and a read that did not land is not evidence.
 *
 * One query per project, so the N tool rows on an Agent's or a Pipeline's
 * Tools panel share ONE request through TanStack's cache rather than issuing
 * one apiece.
 */
import { useQuery } from '@tanstack/react-query';

import { collectCredentialTitles } from '@/entities/credential';

import { getConfigurationsList } from './configurations';

/** The section every toolkit credential lives in (`vectorstorage` is the other value the catalogue serves, and no toolkit references one as a credential). */
const CREDENTIALS_SECTION = 'credentials';

export function useProjectCredentialTitles(projectId: string | undefined, options: { enabled?: boolean } = {}): ReadonlySet<string> | undefined {
  const enabled = (options.enabled ?? true) && projectId !== undefined && projectId !== '';
  const query = useQuery({
    queryKey: ['agents', 'configurations', 'titles', projectId],
    queryFn: async ({ signal }) => {
      const page = await getConfigurationsList(projectId ?? '', CREDENTIALS_SECTION, signal);
      return [...page.items, ...(page.shared?.items ?? [])];
    },
    enabled,
  });
  if (!enabled || query.data === undefined) return undefined;
  return collectCredentialTitles(query.data);
}
