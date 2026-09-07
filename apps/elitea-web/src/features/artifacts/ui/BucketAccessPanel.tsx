/**
 * The composition root for the bucket-access dialog: the three queries, the
 * two writes, and the error message the dialog shows.
 *
 * WHY THIS EXISTS RATHER THAN WIRING IT ON THE PAGE. The page owns URL state
 * and file navigation and nothing else about access. Every piece the dialog
 * needs — the exceptions, the members, the mutations — is fetched here, so the
 * page passes a bucket name and a close callback. That also gives the wiring
 * ONE place a composition-root test can address: `BucketAccessDialog` renders
 * whatever it is handed, so a test of the dialog alone proves nothing about
 * whether anything fetches the rows (the class recorded in #597).
 */
import { useState, type ReactNode } from 'react';

import { t } from '@/shared/i18n';

import {
  useBucketAccessCandidates,
  useBucketAccessMutations,
  useBucketPermissions,
} from '../model/useBucketAccess';
import type { BucketAccess } from '../lib/bucketAccess';
import { BucketAccessDialog } from './BucketAccessDialog';

export interface BucketAccessPanelProps {
  readonly projectId: string | undefined;
  /** The bucket whose exceptions are on screen; `undefined` closes the dialog. */
  readonly bucket?: string | undefined;
  readonly onClose: () => void;
}

export function BucketAccessPanel(props: BucketAccessPanelProps): ReactNode {
  const [error, setError] = useState<string>();
  const permissions = useBucketPermissions(props.bucket === undefined ? undefined : props.projectId);
  const members = useBucketAccessCandidates(props.bucket === undefined ? undefined : props.projectId);
  const rows = permissions.data ?? [];
  const mutations = useBucketAccessMutations(props.projectId, props.bucket, rows);

  if (props.bucket === undefined) return null;

  const loadFailed = permissions.isError
    ? t('artifacts.bucketAccess.error.load', 'Failed to load the bucket exceptions.')
    : undefined;

  return (
    <BucketAccessDialog
      open
      bucket={props.bucket}
      rows={rows}
      candidates={members.candidates}
      isLoading={permissions.isFetching || members.isLoading}
      isSaving={mutations.isSaving}
      {...((error ?? loadFailed) === undefined ? {} : { errorMessage: error ?? loadFailed })}
      onClose={() => {
        setError(undefined);
        props.onClose();
      }}
      onSetAccess={(userId: number, access: BucketAccess) => {
        setError(undefined);
        void mutations.setAccess({ userId, access }).catch(() => {
          setError(t('artifacts.bucketAccess.error.save', 'Failed to save the bucket exception.'));
        });
      }}
      onRemove={(userId: number) => {
        setError(undefined);
        void mutations.removeException(userId).catch(() => {
          setError(t('artifacts.bucketAccess.error.remove', 'Failed to remove the bucket exception.'));
        });
      }}
    />
  );
}
