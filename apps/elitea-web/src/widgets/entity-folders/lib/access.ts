import type { EntityFolderKind } from '@/entities/entity-folder';

/** What a person may do with the folders of a list. */
interface EntityFolderAccess {
  readonly canCreate: boolean;
  readonly canUpdate: boolean;
}

/**
 * The folders are gated by the permissions of the LIST they sit in (issue
 * 2048), the same strings the server's routes ask for: create makes a
 * folder, update renames, reorders, deletes and moves an entity into one.
 * Pipelines are applications, so they use the applications' strings.
 */
const PERMISSIONS_BY_KIND: Readonly<Record<EntityFolderKind, { readonly create: string; readonly update: string }>> = {
  agent: { create: 'models.applications.applications.create', update: 'models.applications.application.update' },
  pipeline: { create: 'models.applications.applications.create', update: 'models.applications.application.update' },
  skill: { create: 'models.applications.skills.create', update: 'models.applications.skills.update' },
  toolkit: { create: 'models.applications.tools.create', update: 'models.applications.tool.update' },
  mcp: { create: 'models.applications.tools.create', update: 'models.applications.tool.update' },
};

export function entityFolderAccess(kind: EntityFolderKind, permissions: ReadonlySet<string>): EntityFolderAccess {
  const needed = PERMISSIONS_BY_KIND[kind];
  return { canCreate: permissions.has(needed.create), canUpdate: permissions.has(needed.update) };
}
