import { useMemo } from 'react';

import { useListApplications } from '@/shared/api/generated/applications/applications';
import type { Application } from '@/shared/api/generated/model';
import { unwrapListPage } from '@/shared/api/unwrap';

/** What a member's avatar draws through the live-avatar renderer: the uploaded icon and the avatar pack. */
export interface MemberAvatar {
  readonly iconUrl?: string | undefined;
  readonly pack?: object | undefined;
}

/** The list route's largest page (`limit` max in the contract): a project's agents in one read. */
const AGENT_PAGE = 1000;

function avatarOf(application: Application): MemberAvatar | undefined {
  const meta = application.meta ?? {};
  const iconMeta = meta['icon_meta'];
  const url = typeof iconMeta === 'object' && iconMeta !== null ? (iconMeta as Record<string, unknown>)['url'] : undefined;
  const iconUrl = typeof url === 'string' && url !== '' ? url : undefined;
  const rawPack = meta['avatar_pack'];
  const pack = typeof rawPack === 'object' && rawPack !== null ? rawPack : undefined;
  return iconUrl === undefined && pack === undefined ? undefined : { iconUrl, pack };
}

/**
 * The avatars of the project's agents by application id (issue 3134): a team
 * member is saved by id and name only, so its card reads the project's agent
 * list (the same row the Agents card draws its avatar from: `meta.icon_meta`
 * and `meta.avatar_pack`). A member from another project, or one the read
 * does not reach, gets the monogram.
 */
export function useTeamMemberAvatars(projectId: string): ReadonlyMap<number, MemberAvatar> {
  const list = useListApplications(projectId, { agents_type: 'classic', limit: AGENT_PAGE }, { query: { enabled: projectId !== '' } });
  return useMemo(() => {
    const { rows } = unwrapListPage<Application>(list.data, 'listApplications(team avatars)');
    const avatars = new Map<number, MemberAvatar>();
    for (const row of rows) {
      const avatar = avatarOf(row);
      if (avatar !== undefined) avatars.set(Number(row.id), avatar);
    }
    return avatars;
  }, [list.data]);
}
