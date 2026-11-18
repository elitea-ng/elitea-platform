/**
 * Which of the skills a picker lists are untrusted (ADR-0051 amendment 1, A1.6), so the list can say so before a
 * person picks one: the text of a skill written in another project reaches the model as quoted data until this
 * project trusts it.
 *
 * A pinned skill carries its own trust state in the pin. The other project skills have none in the list, so the first
 * few the list shows are asked (`getSkillTrust`, cached for a minute). A skill whose state is not known is not marked:
 * the badge says what the server said, never a guess.
 */
import { useMemo } from 'react';

import { useQueries } from '@tanstack/react-query';

import type { ConversationSkillPin } from '@/entities/conversation-skill';
import { getSkillTrust } from '@/shared/api/generated/skills/skills';
import { unwrapBody } from '@/shared/api/unwrap';

/** How many unpinned rows are asked about at once: the picker shows a screenful, not the whole project. */
export const TRUST_LOOKUP_LIMIT = 12;

interface TrustBody {
  readonly state?: string;
}

/** One boolean per asked id; a module-level function so the combined answer keeps its identity between renders. */
function combineAnswers(results: readonly { readonly data?: boolean | undefined }[]): readonly boolean[] {
  return results.map((result) => result.data === true);
}

/** The ids of the skills whose text is untrusted, among `pins` and the first `ids` the picker shows. */
export function useUntrustedSkillIds(
  projectId: string | undefined,
  pins: readonly ConversationSkillPin[],
  ids: readonly string[],
  enabled: boolean,
): ReadonlySet<string> {
  const pinned = useMemo(() => new Map(pins.flatMap((pin) => (pin.skill_id === undefined ? [] : [[String(pin.skill_id), pin.trust.state === 'untrusted'] as const]))), [pins]);
  const asked = useMemo(() => ids.filter((id) => !pinned.has(id)).slice(0, TRUST_LOOKUP_LIMIT), [ids, pinned]);
  const answers = useQueries({
    combine: combineAnswers,
    queries: asked.map((id) => ({
      queryKey: ['skill-picker-trust', projectId ?? '', id] as const,
      queryFn: async (): Promise<boolean> => (unwrapBody(await getSkillTrust(projectId ?? '', Number(id))) as TrustBody).state === 'untrusted',
      enabled: enabled && projectId !== undefined && projectId !== '' && Number.isInteger(Number(id)),
      staleTime: 60_000,
      retry: false,
    })),
  });
  return useMemo(() => {
    const untrusted = new Set<string>();
    for (const [id, isUntrusted] of pinned) if (isUntrusted) untrusted.add(id);
    asked.forEach((id, index) => {
      if (answers[index] === true) untrusted.add(id);
    });
    return untrusted;
  }, [pinned, asked, answers]);
}
