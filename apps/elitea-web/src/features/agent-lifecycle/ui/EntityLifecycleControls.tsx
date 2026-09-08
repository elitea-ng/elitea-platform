import type { ReactNode } from 'react';
import { useCallback, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { handleCopy } from '@/shared/lib/clipboard';
import { t } from '@/shared/i18n';

import { buildEntityShareLink, getLifecycleBasename, type LifecycleEntity } from '../lib/shareLink';
import { lifecycleErrorMessage, useEntityLifecycle } from '../model/useEntityLifecycle';
import { EntityLifecycleMenu } from './EntityLifecycleMenu';
import { ForkEntityDialog, type ForkTargetProject } from './ForkEntityDialog';
import { PublishVersionDialog } from './PublishVersionDialog';

/**
 * The lifecycle controls, mounted on BOTH editors.
 *
 * `projects` is a PROP because the only source of a project list in this app
 * is `useProjectOptions`, which lives in `widgets/sidebar` — a layer a
 * `features/` slice may not import (`no-upward-from-features`). The two pages
 * that mount this are above both layers and call the hook themselves; two
 * lines of duplication at the call sites is the price of not putting a widget
 * dependency inside a feature.
 *
 * `entity` is what makes the same component serve agents and pipelines. The
 * one behavioural difference is publish: the server refuses a pipeline with
 * 400 `pipeline_not_publishable`, so the pipeline copy renders no publish and
 * no unpublish item at all rather than a control whose only outcome is a
 * refusal.
 */
export interface EntityLifecycleControlsProps {
  readonly entity: LifecycleEntity;
  readonly projectId: string | undefined;
  /** Fork targets, from the mounting page's `useProjectOptions`. */
  readonly projects: readonly ForkTargetProject[];
  readonly entityId: string | undefined;
  readonly entityName: string;
  /** The list tab the editor was opened from; the share link points back at it. */
  readonly tab: string | undefined;
  /** `application_versions.id`, a STRING on the wire ("Numeric id serialized as string"). */
  readonly activeVersionId: string | undefined;
  /** `application_versions.status` of the open version. */
  readonly activeVersionStatus: string | undefined;
  /** Adds an Export item to the menu's ENTITY group. Omitted where the editor already carries an Export button. */
  readonly onExport?: () => void;
  /** Adds a Delete item to the menu's ENTITY group. The caller owns the confirmation dialog. */
  readonly onDelete?: () => void;
}

export function EntityLifecycleControls({
  entity,
  projectId,
  projects,
  entityId,
  entityName,
  tab,
  activeVersionId,
  activeVersionStatus,
  onExport,
  onDelete,
}: EntityLifecycleControlsProps): ReactNode {
  const [notice, setNotice] = useState<string>();
  const [error, setError] = useState<string>();
  const [publishOpen, setPublishOpen] = useState(false);
  const [forkOpen, setForkOpen] = useState(false);
  const lifecycle = useEntityLifecycle(projectId, entityId);

  const share = useCallback(
    (versionId: number | undefined) => {
      if (entityId === undefined) return;
      const link = buildEntityShareLink({
        origin: window.location.origin,
        basename: getLifecycleBasename(),
        entity,
        tab: tab ?? 'all',
        entityId,
        ...(versionId === undefined ? {} : { versionId: String(versionId) }),
        name: entityName,
      });
      void handleCopy(link);
      setError(undefined);
      setNotice(t('features.agentLifecycle.notice.linkCopied', 'The link has been copied to the clipboard.'));
    },
    [entity, entityId, entityName, tab],
  );

  // One conversion, at this boundary. Every lifecycle route takes the version
  // id as a path integer; an unparseable id disables the whole version group
  // rather than sending `NaN` into a URL.
  const numericVersionId = activeVersionId === undefined ? undefined : Number(activeVersionId);
  const versionId = numericVersionId !== undefined && Number.isFinite(numericVersionId) ? numericVersionId : undefined;
  const canPublish = entity === 'agents' && versionId !== undefined;
  const isPublished = activeVersionStatus === 'published';

  return (
    <Box sx={wrapperSx}>
      {notice !== undefined && (
        <Typography
          component="output"
          variant="bodySmall"
          data-testid="lifecycle-notice"
        >
          {notice}
        </Typography>
      )}
      {error !== undefined && (
        <Typography
          role="alert"
          variant="bodySmall"
          data-testid="lifecycle-error"
        >
          {error}
        </Typography>
      )}
      <EntityLifecycleMenu
        testIdPrefix={entity === 'agents' ? 'agent' : 'pipeline'}
        disabled={entityId === undefined}
        isPublished={isPublished}
        {...(versionId === undefined ? {} : { onShareVersion: () => share(versionId) })}
        onShareEntity={() => share(undefined)}
        {...entityActionHandlers(onExport, onDelete)}
        onFork={() => {
          setError(undefined);
          setNotice(undefined);
          setForkOpen(true);
        }}
        {...(canPublish
          ? {
              onPublish: () => {
                setError(undefined);
                setNotice(undefined);
                lifecycle.validate.reset();
                lifecycle.publish.reset();
                setPublishOpen(true);
              },
              onUnpublish: () => {
                setError(undefined);
                lifecycle.unpublish.mutate(versionId, {
                  onSuccess: () =>
                    setNotice(t('features.agentLifecycle.notice.unpublished', 'This version is no longer published.')),
                  onError: (cause) => setError(lifecycleErrorMessage(cause)),
                });
              },
            }
          : {})}
      />
      <PublishVersionDialog
        open={publishOpen}
        validation={lifecycle.validate.data}
        isValidating={lifecycle.validate.isPending}
        isPublishing={lifecycle.publish.isPending}
        error={firstErrorMessage(lifecycle.validate.error, lifecycle.publish.error)}
        onClose={() => setPublishOpen(false)}
        onValidate={(versionName, category) => {
          if (versionId === undefined) return;
          lifecycle.validate.mutate({ versionId, versionName, category });
        }}
        onPublish={(versionName, category, validationToken) => {
          if (versionId === undefined) return;
          lifecycle.publish.mutate(
            { versionId, versionName, category, validationToken },
            {
              onSuccess: (outcome) => {
                setPublishOpen(false);
                // The catalogue ids are the proof the entry reached ELITEA
                // Catalog and not only the Published tab. Saying "published"
                // without them would repeat the split this release closed.
                setNotice(
                  outcome.catalogVersionId === undefined
                    ? t('features.agentLifecycle.notice.published', 'This version is published.')
                    : t('features.agentLifecycle.notice.publishedToCatalog', 'This version is published to the Catalog.'),
                );
              },
              onError: (cause) => setError(lifecycleErrorMessage(cause)),
            },
          );
        }}
      />
      <ForkEntityDialog
        open={forkOpen}
        entityName={entityName}
        entityType={entity === 'agents' ? 'agent' : 'pipeline'}
        projects={projects}
        defaultProjectId={projectId}
        isForking={lifecycle.fork.isPending}
        error={firstErrorMessage(lifecycle.fork.error, null)}
        onClose={() => setForkOpen(false)}
        onFork={(targetProjectId) => {
          if (entityId === undefined) return;
          lifecycle.fork.mutate(
            { entityId: Number(entityId), targetProjectId },
            {
              onSuccess: (outcome) => {
                setForkOpen(false);
                // 207 means SOME of it forked. Reporting that as a plain
                // success hides the errors channel the server just sent.
                setNotice(
                  outcome.status === 207
                    ? t('features.agentLifecycle.notice.forkedPartly', 'The copy was created, but part of it was not copied.')
                    : t('features.agentLifecycle.notice.forked', 'A copy was created in the chosen project.'),
                );
              },
              onError: (cause) => setError(lifecycleErrorMessage(cause)),
            },
          );
        }}
      />
    </Box>
  );
}

/**
 * The two optional ENTITY-group handlers, as one spreadable object.
 *
 * Lives outside the component for the §3.5 cyclomatic-complexity budget (12),
 * which two inline `undefined` checks take the component past — the same
 * reason `pages/agents/ui/EditApplicationActions.tsx` moved its own optional
 * unwrapping out of its call site.
 */
function entityActionHandlers(
  onExport: (() => void) | undefined,
  onDelete: (() => void) | undefined,
): { onExport?: () => void; onDelete?: () => void } {
  return {
    ...(onExport === undefined ? {} : { onExport }),
    ...(onDelete === undefined ? {} : { onDelete }),
  };
}

/**
 * The first of two mutations that failed, as text.
 *
 * Both halves of the publish wizard can fail, and the dialog has one error
 * slot. Validation runs first, so its failure is the one that explains the
 * state the reader is looking at.
 */
function firstErrorMessage(primary: Error | null, secondary: Error | null): string | undefined {
  if (primary !== null) return lifecycleErrorMessage(primary);
  if (secondary !== null) return lifecycleErrorMessage(secondary);
  return undefined;
}

const wrapperSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.25rem' };
