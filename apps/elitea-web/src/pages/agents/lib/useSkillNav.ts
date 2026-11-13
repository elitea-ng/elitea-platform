import { useMemo } from 'react';

import { useNavigate, useRouter } from '@tanstack/react-router';

interface SkillNav {
  readonly createNew: () => void;
  readonly openInNewTab: (skillId: number) => void;
}

/**
 * The agent editor's two ways out to a skill: the picker's "Create new"
 * (the New Skill page) and a skill row's open-in-new-tab icon. Built on the
 * router so the URL carries the deployment's base path.
 */
export function useSkillNav(): SkillNav {
  const navigate = useNavigate();
  const router = useRouter();
  return useMemo(
    () => ({
      createNew: () => {
        void navigate({ to: '/skills/create' });
      },
      openInNewTab: (skillId: number) => {
        const location = router.buildLocation({ to: '/skills/$tab/$skillId', params: { tab: 'all', skillId: String(skillId) } });
        window.open(`${window.location.origin}${router.history.createHref(location.href)}`, '_blank');
      },
    }),
    [navigate, router],
  );
}
