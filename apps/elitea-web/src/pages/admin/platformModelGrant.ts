/**
 * A platform model's GRANT, as this screen reads and writes it.
 *
 * ## What the grant is
 *
 * A platform model used to be one thing: published, and therefore offered to
 * every project on the deployment. The grant splits that into three — every
 * project, no project, or a chosen set of projects — while `shared = true` goes
 * on meaning "this is a platform row", so nothing that keys on it changes.
 *
 * ## Why "no scope" reads as "all projects"
 *
 * Every row written before the grant existed carries no scope, and every one of
 * them was offered to every project. The server reports such a row as `all` for
 * that reason, and this reader repeats the rule rather than trusting it,
 * because the same screen has to work against a server that predates the field
 * and sends nothing at all. A blank there would render as "granted to nobody"
 * for every model on that deployment — the opposite of the truth, on the screen
 * an operator would act on.
 */
import { t } from '@/shared/i18n';

import type { PlatformModel } from './api/adminLlmPlatformModelsApi';

/** The three grants a platform model can carry. */
export type ShareScope = 'all' | 'none' | 'projects';

/** The scope a row with none reads as. */
export const DEFAULT_SHARE_SCOPE: ShareScope = 'all';

const SHARE_SCOPES: readonly ShareScope[] = ['all', 'none', 'projects'];

/** One row's scope, defaulting anything this screen does not know to `all`. */
export function shareScopeOf(model: Pick<PlatformModel, 'share_scope'>): ShareScope {
  const scope = model.share_scope;
  return SHARE_SCOPES.find((known) => known === scope) ?? DEFAULT_SHARE_SCOPE;
}

/** One row's granted project ids, as a plain array the form can hold. */
export function sharedWithOf(model: Pick<PlatformModel, 'shared_with'>): readonly number[] {
  return (model.shared_with ?? []).filter((id) => Number.isInteger(id) && id > 0);
}

/** The words each choice gets in the dialog's select. */
export function shareScopeLabel(scope: ShareScope): string {
  if (scope === 'none') return t('pages.admin.platformModels.scope.none', 'No project');
  if (scope === 'projects') {
    return t('pages.admin.platformModels.scope.projects', 'Selected projects');
  }
  return t('pages.admin.platformModels.scope.all', 'All projects');
}

/** Every choice, in the order the select offers them. */
export function shareScopeOptions(): readonly ShareScope[] {
  return SHARE_SCOPES;
}

/**
 * The chip the table shows.
 *
 * A `projects` grant reports the COUNT rather than the ids: the table is read
 * to find the model that is restricted, and a row of ids is a second question
 * the edit dialog answers.
 */
export function shareScopeChipLabel(scope: ShareScope, granted: number): string {
  if (scope === 'none') return t('pages.admin.platformModels.scope.chipNone', 'No project');
  if (scope === 'projects') {
    return t('pages.admin.platformModels.scope.chipProjects', '{{count}} projects', {
      count: granted,
    });
  }
  return t('pages.admin.platformModels.scope.chipAll', 'All projects');
}

/**
 * The chip's colour. A restricted model is not an error and not a success: it
 * is a deliberate state, and `none` is the one an operator most often reaches
 * by accident — a model granted to nobody is stored, listed and served to no
 * caller.
 */
export function shareScopeChipColour(scope: ShareScope): 'default' | 'info' | 'warning' {
  if (scope === 'none') return 'warning';
  if (scope === 'projects') return 'info';
  return 'default';
}
