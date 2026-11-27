import { authorDisplayName, isDeletedAuthor } from '@/entities/author';
import type { Application } from '@/shared/api/generated/model';

import type { ApplicationListRow } from '../ui/ApplicationListPanel';

/** Same "Untitled" fallback `entities/application`'s `applicationDisplayName` selector encodes, over the GENERATED snake_case `Application`. */
export function applicationName(application: Application): string {
  return application.name.trim() !== '' ? application.name : 'Untitled';
}

/**
 * #915 — the SOURCE agent's id, when this row is a fork. `Application.meta`
 * is a permissive passthrough bag; `repos/applications.go`'s `List` merges
 * `parent_entity_id` into it exactly when `is_forked` is true.
 */
function forkedFromId(application: Application): string | undefined {
  if (!application.is_forked) return undefined;
  const parentEntityId = application.meta?.['parent_entity_id'];
  return typeof parentEntityId === 'string' || typeof parentEntityId === 'number' ? String(parentEntityId) : undefined;
}

/** The uploaded icon the list row carries on `meta.icon_meta` (#1995). */
function iconUrlOf(application: Application): string | undefined {
  const iconMeta = application.meta?.['icon_meta'];
  if (typeof iconMeta !== 'object' || iconMeta === null) return undefined;
  const url = (iconMeta as Record<string, unknown>)['url'];
  return typeof url === 'string' && url !== '' ? url : undefined;
}

/** A string the list row carries on its passthrough `meta`, or undefined. */
function metaText(application: Application, key: string): string | undefined {
  const value = application.meta?.[key];
  return typeof value === 'string' && value.trim() !== '' ? value : undefined;
}

/** The version's avatar pack the list row carries on `meta.avatar_pack` (#3133): the card's live avatar. */
function avatarPackOf(application: Application): object | undefined {
  const pack = application.meta?.['avatar_pack'];
  return typeof pack === 'object' && pack !== null ? pack : undefined;
}

/** One list row from the wire; `badge` is the status as a word (ADR-0051: status is a badge, not a tab). */
export function toApplicationRow(
  application: Application,
  onSelectForkedFrom: (id: string) => void,
  badge?: string,
  action?: ApplicationListRow['action'],
): ApplicationListRow {
  const forkedFrom = forkedFromId(application);
  const iconUrl = iconUrlOf(application);
  const personaRole = metaText(application, 'persona_role');
  const avatarPack = avatarPackOf(application);
  return {
    id: application.id,
    name: applicationName(application),
    description: application.description ?? '',
    // A deleted author's account answers no name or email (#6702).
    authors: (application.authors ?? []).map((author) => ({
      id: author.id,
      name: authorDisplayName(author),
      deleted: isDeletedAuthor(author),
    })),
    tags: application.tags ?? [],
    createdAt: application.created_at,
    updatedAt: application.updated_at,
    ...(personaRole === undefined ? {} : { personaRole }),
    ...(avatarPack === undefined ? {} : { avatarPack }),
    ...(badge === undefined ? {} : { badge }),
    ...(action === undefined ? {} : { action }),
    ...(iconUrl === undefined ? {} : { iconUrl }),
    ...(forkedFrom === undefined ? {} : { forkedFrom: { onClick: () => onSelectForkedFrom(forkedFrom) } }),
  };
}

export function matchesApplicationQuery(application: Application, query: string): boolean {
  const needle = query.trim().toLowerCase();
  if (needle === '') return true;
  return application.name.toLowerCase().includes(needle);
}
