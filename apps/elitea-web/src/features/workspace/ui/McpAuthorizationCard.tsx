/**
 * An MCP server a remote tool call reaches needs the person's sign-in
 * (`approval_request` of kind `mcp_authorization`, issue 1231). The sign-in
 * happens in the system browser, on the web app's page for that toolkit (the
 * flow the web chat's `mcp_authorization_required` frame drives; no embedded
 * browser, ADR-0029 decision 9): "Authorize in browser" is a `target="_blank"`
 * link to the host-built path, which the desktop's external-link handler opens
 * against the connected deployment. The host then waits for Continue (the same
 * tool call is sent again) or Cancel (only that call fails; the turn goes on).
 *
 * Nothing here holds a token: the host sends the toolkit's name, the server's
 * address and the page to open, never what the browser signs in with.
 */
import { useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import type { ApprovalDecision, ApprovalRequestPayload } from '@/shared/desktop/workspaceIpc';

export interface McpAuthorizationCardProps {
  request: ApprovalRequestPayload;
  respond: (decision: ApprovalDecision) => void;
}

/** The host builds `authorize_path` from numbers it holds; anything else is not opened. */
const SAFE_PATH = /^\/app\/\d+\/(mcps|toolkits)\/all\/\d+$/;

function hintText(toolkit: string, path: string | undefined, opened: boolean): string {
  if (path === undefined) return t('workspace.mcpAuthorization.inWebApp', 'Sign in to {{toolkit}} in the Elitea web app, then continue.', { toolkit });
  return opened
    ? t('workspace.mcpAuthorization.waiting', 'Finish signing in in your browser, then continue.')
    : t('workspace.mcpAuthorization.hint', 'Sign in in your browser; the agent waits here and then runs the same tool again.');
}

export function McpAuthorizationCard({ request, respond }: McpAuthorizationCardProps): React.JSX.Element {
  const [opened, setOpened] = useState(false);
  const toolkit = request.toolkit !== undefined && request.toolkit !== '' ? request.toolkit : request.tool;
  const path = request.authorize_path !== undefined && SAFE_PATH.test(request.authorize_path) ? request.authorize_path : undefined;
  const serverUrl = request.server_url ?? '';
  const titleId = `workspace-mcp-authorization-${request.request_id}`;
  const hint = hintText(toolkit, path, opened);
  return (
    <Box
      component="section"
      aria-labelledby={titleId}
      data-testid="approval-mcp-authorization-card"
      sx={(theme: Theme) => ({
        display: 'flex',
        flexDirection: 'column',
        gap: 1,
        padding: 1.5,
        border: `1px solid ${theme.vars.palette.divider}`,
        borderRadius: theme.vars.shape.radiusMd,
      })}
    >
      <Typography id={titleId} variant="labelMedium">
        {t('workspace.mcpAuthorization.title', '{{toolkit}} needs your sign-in', { toolkit })}
      </Typography>
      {serverUrl !== '' && (
        <Typography variant="bodySmall" data-selectable sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary, wordBreak: 'break-all' })}>
          {serverUrl}
        </Typography>
      )}
      <Typography variant="bodySmall" component="output" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary })}>
        {hint}
      </Typography>
      <Box sx={{ display: 'flex', gap: 1, flexWrap: 'wrap' }}>
        {path !== undefined && (
          <Button size="small" variant={opened ? 'outlined' : 'contained'} href={path} target="_blank" rel="noopener noreferrer" onClick={() => setOpened(true)}>
            {t('workspace.mcpAuthorization.authorize', 'Authorize in browser')}
          </Button>
        )}
        <Button size="small" variant={opened || path === undefined ? 'contained' : 'outlined'} onClick={() => respond('continue')}>
          {t('workspace.mcpAuthorization.continue', 'Continue')}
        </Button>
        <Button size="small" onClick={() => respond('cancel')}>
          {t('workspace.mcpAuthorization.cancel', 'Cancel')}
        </Button>
      </Box>
    </Box>
  );
}
