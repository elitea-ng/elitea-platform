import { useMemo } from 'react';

import { useRouter } from '@tanstack/react-router';

interface SkillNav {
  readonly createNew: () => void;
  readonly openInNewTab: (skillId: number) => void;
}

/**
 * The agent editor's two ways out to a skill, both in a NEW tab so the editor
 * (and its unsaved edits) stays put: the picker's "Create new" and a skill
 * row's open icon. The URL goes through the router's own href helper, so the
 * deployment's base path (or a hash history) is honoured.
 */
export function useSkillNav(): SkillNav {
  const router = useRouter();
  return useMemo(() => {
    const openInTab = (location: ReturnType<typeof router.buildLocation>): void => {
      const href = router.history.createHref(location.href);
      window.open(new URL(href, window.location.origin).toString(), '_blank', 'noopener,noreferrer');
    };
    return {
      createNew: () => openInTab(router.buildLocation({ to: '/skills/create' })),
      // The detail route's `$tab` segment is required (`/skills/$tab/$skillId`); `all` is the list every skill is in.
      openInNewTab: (skillId: number) => openInTab(router.buildLocation({ to: '/skills/$tab/$skillId', params: { tab: 'all', skillId: String(skillId) } })),
    };
  }, [router]);
}
