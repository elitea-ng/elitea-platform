/**
 * WebhookDeliveriesPanel — the "Recent deliveries" content of one webhook's
 * expandable row (#876's second half). Fetches its own data: mounted only
 * while its row is expanded (see WebhooksTable's Collapse), so there is no
 * reason to pull the list into the parent's own query waterfall.
 */
import { useCallback, useState } from 'react';

import { useMutation, useQueryClient } from '@tanstack/react-query';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import CircularProgress from '@mui/material/CircularProgress';
import IconButton from '@mui/material/IconButton';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import ReplayIcon from '@mui/icons-material/Replay';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import type { WebhookDelivery, ListWebhookDeliveries200 } from '@/shared/api/generated/model';
import {
  getListWebhookDeliveriesQueryKey,
  redeliverWebhookDelivery,
  useListWebhookDeliveries,
} from '@/shared/api/generated/webhooks/webhooks';

export interface WebhookDeliveriesPanelProps {
  readonly projectId: string;
  readonly webhookId: string;
  readonly canRedeliver: boolean;
}

const EMPTY_DELIVERIES: WebhookDelivery[] = [];

const rootSx: SxProps<Theme> = { padding: '0.75rem 1rem 1rem 1rem' };
const emptySx: SxProps<Theme> = { color: 'text.secondary', padding: '0.5rem 0' };
const loadingSx: SxProps<Theme> = { display: 'flex', justifyContent: 'center', padding: '1rem' };

function statusColor(status: WebhookDelivery['status']): 'success' | 'error' | 'warning' | 'default' {
  if (status === 'success') return 'success';
  if (status === 'failed') return 'error';
  // 'blocked' is a destination the platform's SSRF guard refused to dial
  // (loopback, private, link-local or a metadata address) — a configuration
  // problem, not a transient delivery failure, so it reads as a warning
  // rather than the same red as an ordinary failure.
  if (status === 'blocked') return 'warning';
  return 'default';
}

export function WebhookDeliveriesPanel({ projectId, webhookId, canRedeliver }: WebhookDeliveriesPanelProps) {
  const queryClient = useQueryClient();
  const listQuery = useListWebhookDeliveries(projectId, webhookId, { query: { enabled: !!projectId && !!webhookId } });
  const body = listQuery.data?.data as ListWebhookDeliveries200 | undefined;
  const deliveries = body?.items ?? EMPTY_DELIVERIES;

  const [redeliveringId, setRedeliveringId] = useState<string | null>(null);

  const invalidate = useCallback(
    () => void queryClient.invalidateQueries({ queryKey: getListWebhookDeliveriesQueryKey(projectId, webhookId) }),
    [queryClient, projectId, webhookId],
  );

  const redeliverMutation = useMutation({
    mutationFn: (deliveryId: string) => redeliverWebhookDelivery(projectId, webhookId, deliveryId),
    onSuccess: invalidate,
    onSettled: () => setRedeliveringId(null),
  });

  const redeliver = useCallback(
    (deliveryId: string) => {
      setRedeliveringId(deliveryId);
      redeliverMutation.mutate(deliveryId);
    },
    [redeliverMutation],
  );

  if (listQuery.isLoading) {
    return (
      <Box sx={loadingSx}>
        <CircularProgress size={20} />
      </Box>
    );
  }

  if (deliveries.length === 0) {
    return (
      <Box sx={rootSx}>
        <Typography variant="bodySmall2" sx={emptySx}>
          {t('entities.webhook.deliveries.empty', 'No deliveries yet. This webhook fires the next time one of its events happens.')}
        </Typography>
      </Box>
    );
  }

  return (
    <Box sx={rootSx} data-testid={`webhook-deliveries-${webhookId}`}>
      <Table size="small">
        <TableHead>
          <TableRow>
            <TableCell>{t('entities.webhook.deliveries.event', 'Event')}</TableCell>
            <TableCell>{t('entities.webhook.deliveries.status', 'Status')}</TableCell>
            <TableCell>{t('entities.webhook.deliveries.attempts', 'Attempts')}</TableCell>
            <TableCell>{t('entities.webhook.deliveries.response', 'Response')}</TableCell>
            <TableCell>{t('entities.webhook.deliveries.when', 'When')}</TableCell>
            <TableCell align="right">{t('entities.webhook.deliveries.actions', 'Actions')}</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {deliveries.map((delivery) => (
            <TableRow key={delivery.id} data-testid={`webhook-delivery-${delivery.id}`}>
              <TableCell>
                <Typography variant="bodySmall2">{delivery.event}</Typography>
                {delivery.redelivery_of ? (
                  <Typography variant="bodySmall2" color="text.secondary">
                    {t('entities.webhook.deliveries.redeliveryOf', 'Redelivery of {{id}}', { id: delivery.redelivery_of })}
                  </Typography>
                ) : null}
              </TableCell>
              <TableCell>
                <Chip
                  size="small"
                  label={delivery.status}
                  color={statusColor(delivery.status)}
                  data-testid={`webhook-delivery-status-${delivery.id}`}
                />
              </TableCell>
              <TableCell>{delivery.attempts}</TableCell>
              <TableCell>
                {delivery.response_code ?? t('entities.webhook.deliveries.noResponse', 'No response')}
                {delivery.last_error ? (
                  <Typography variant="bodySmall2" color="error.main" title={delivery.last_error}>
                    {delivery.last_error}
                  </Typography>
                ) : null}
              </TableCell>
              <TableCell>
                <Typography variant="bodySmall2" title={delivery.created_at}>
                  {new Date(delivery.created_at).toLocaleString()}
                </Typography>
              </TableCell>
              <TableCell align="right">
                {canRedeliver && delivery.status !== 'blocked' && (
                  <Tooltip title={t('entities.webhook.deliveries.redeliver', 'Redeliver')}>
                    <span>
                      <IconButton
                        size="small"
                        onClick={() => redeliver(delivery.id)}
                        disabled={redeliveringId === delivery.id}
                        aria-label={t('entities.webhook.deliveries.redeliverAria', 'Redeliver this event')}
                        data-testid={`webhook-redeliver-${delivery.id}`}
                      >
                        {redeliveringId === delivery.id ? <CircularProgress size={16} /> : <ReplayIcon fontSize="small" />}
                      </IconButton>
                    </span>
                  </Tooltip>
                )}
                {canRedeliver && delivery.status === 'blocked' && (
                  <Tooltip
                    title={t(
                      'entities.webhook.deliveries.redeliverBlocked',
                      'This destination was refused by the platform and cannot be redelivered. Update the webhook URL first.',
                    )}
                  >
                    <span>
                      <IconButton size="small" disabled aria-label={t('entities.webhook.deliveries.redeliverAria', 'Redeliver this event')}>
                        <ReplayIcon fontSize="small" />
                      </IconButton>
                    </span>
                  </Tooltip>
                )}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </Box>
  );
}
