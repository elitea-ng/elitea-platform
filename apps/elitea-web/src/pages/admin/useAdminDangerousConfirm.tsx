/**
 * One pending dangerous action of a page, confirmed in
 * `AdminDangerousActionDialog` (ADR-0059 decision 6; R3.8b, issue 2614).
 *
 * A control calls `ask` with what the dialog says and a `run` that sends the
 * request with the reason; the page renders `dialog` once. A page with a
 * single dangerous control renders the dialog itself instead.
 */
import { useState, type ReactNode } from 'react';

import { AdminDangerousActionDialog, type AdminDangerousActionDialogProps } from './AdminDangerousActionDialog';
import type { AdminReason } from './adminDangerousAction';

export interface AdminDangerousRequest
  extends Pick<AdminDangerousActionDialogProps, 'action' | 'title' | 'change' | 'reach' | 'target' | 'confirmLabel'> {
  /** Sends the request with the reason the dialog collected. */
  readonly run: (reason: AdminReason) => void;
}

export function useAdminDangerousConfirm(): {
  readonly ask: (request: AdminDangerousRequest) => void;
  readonly dialog: ReactNode;
} {
  const [request, setRequest] = useState<AdminDangerousRequest | undefined>(undefined);
  const dialog =
    request === undefined ? null : (
      <AdminDangerousActionDialog
        open
        action={request.action}
        title={request.title}
        change={request.change}
        reach={request.reach}
        target={request.target}
        confirmLabel={request.confirmLabel}
        pending={false}
        onClose={() => setRequest(undefined)}
        onConfirm={(reason) => {
          setRequest(undefined);
          request.run(reason);
        }}
      />
    );
  return { ask: setRequest, dialog };
}
