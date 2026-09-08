/**
 * The two non-list states every evaluation sub-view shares: a failed read, and
 * a read that legitimately returned nothing.
 *
 * BOTH ARE REPORTED, and that is the whole point of the component. A list with
 * no rows and no message is indistinguishable from a listing that failed
 * silently — the reading every "200 with an empty screen" defect in this
 * application produces. Repeating the pair in three sub-views is how one of
 * them ends up without it.
 */
import type { ReactNode } from 'react';

import Typography from '@mui/material/Typography';

export interface EvaluationStatusProps {
  readonly isError: boolean;
  readonly isEmpty: boolean;
  readonly errorText: string;
  readonly emptyText: string;
}

export function EvaluationStatus(props: EvaluationStatusProps): ReactNode {
  const { isError, isEmpty, errorText, emptyText } = props;

  if (isError) {
    return (
      <Typography role="alert" variant="bodyMedium">
        {errorText}
      </Typography>
    );
  }
  if (isEmpty) {
    // `<output>` and not `<div role="status">`: oxlint's
    // jsx-a11y/prefer-tag-over-role refuses the attribute where a semantic tag
    // exists, and routes/-ui/RouteStatus.tsx already settles the pattern for
    // this app. The live region is the point — an empty list that arrives after
    // a load has to be ANNOUNCED, not merely painted.
    return (
      <Typography component="output" variant="bodyMedium">
        {emptyText}
      </Typography>
    );
  }
  return null;
}
