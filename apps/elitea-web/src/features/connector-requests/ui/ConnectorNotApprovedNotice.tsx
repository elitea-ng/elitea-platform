/**
 * Where a toolkit or MCP server is refused because its connector is not
 * approved (the `approved_only` catalogue mode, ADR-0044 decision 1, #1501):
 * says so, and offers "Request connector". After the request is sent the
 * notice says it is waiting for an approver.
 */
import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';

import type { ConnectorDeclaredTool, ConnectorRequest } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import type { ConnectorTarget } from '../api/connectorRequests';
import { ConnectorRequestDialog } from './ConnectorRequestDialog';

export interface ConnectorNotApprovedNoticeProps {
  readonly projectId: string | undefined;
  readonly target: ConnectorTarget;
  readonly tools?: readonly ConnectorDeclaredTool[] | undefined;
}

export function ConnectorNotApprovedNotice({ projectId, target, tools }: ConnectorNotApprovedNoticeProps): ReactNode {
  const [open, setOpen] = useState(false);
  const [sent, setSent] = useState<ConnectorRequest>();

  if (sent !== undefined) {
    return (
      <Alert severity="info" data-testid="connector-request-sent">
        {t(
          'features.connectorRequests.notice.sent',
          'Connector request #{{id}} was sent. You can use the connector once an approver approves it.',
          { id: sent.id },
        )}
      </Alert>
    );
  }
  return (
    <>
      <Alert
        severity="warning"
        data-testid="connector-not-approved"
        action={
          <Button color="inherit" size="small" onClick={() => setOpen(true)} data-testid="connector-request-open">
            {t('features.connectorRequests.notice.request', 'Request connector')}
          </Button>
        }
      >
        {t(
          'features.connectorRequests.notice.text',
          '{{target}} is not an approved connector for this project. Request it, and a connector approver reviews the request.',
          { target: target.target },
        )}
      </Alert>
      {open && (
        <ConnectorRequestDialog
          open
          projectId={projectId}
          target={target}
          tools={tools}
          onClose={() => setOpen(false)}
          onSubmitted={(request) => {
            setOpen(false);
            setSent(request);
          }}
        />
      )}
    </>
  );
}
