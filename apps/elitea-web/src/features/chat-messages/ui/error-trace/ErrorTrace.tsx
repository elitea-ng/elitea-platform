/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/ui/error-trace/
 * ErrorTrace.jsx` — renders an error trace for a failed message.
 *
 * Port of `apps/elitea-ui/src/[fsd]/features/chat/ui/error-trace/
 * ErrorTrace.jsx`.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import DownloadOutlinedIcon from '@mui/icons-material/DownloadOutlined';
import ErrorOutlinedIcon from '@mui/icons-material/ErrorOutlined';
import Box from '@mui/material/Box';
import Collapse from '@mui/material/Collapse';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { handleCopy } from '@/shared/lib/clipboard';
import { triggerBlobDownload } from '@/shared/lib/download';
import type { Theme } from '@mui/material/styles';

/** @public Props for `ErrorTrace`. */
export interface ErrorTraceProps {
  /** The error message/exception to display. */
  readonly error: unknown;
  /** Whether to expand the trace by default. */
  readonly defaultExpanded?: boolean;
}

/** The always-visible headline (baseline: `headline`). */
function errorHeadline(error: unknown): string {
  if (typeof error === 'string') return error;
  if (error instanceof Error) return error.message;
  return JSON.stringify(error);
}

/** Supplementary stack/detail shown only inside the collapse (baseline: `trace`), `undefined` when there is nothing beyond the headline. */
function errorDetail(error: unknown, headline: string): string | undefined {
  if (error instanceof Error) return error.stack;
  if (typeof error === 'string') return undefined;
  const detail = JSON.stringify(error, null, 2);
  return detail !== headline ? detail : undefined;
}

/**
 * `ErrorTrace` — renders an always-visible error headline, plus (when there
 * is supplementary detail beyond the headline) an expandable trace panel
 * with download/copy actions.
 */
export function ErrorTrace({ error, defaultExpanded = false }: ErrorTraceProps): ReactNode {
  const [expanded, setExpanded] = useState(defaultExpanded);

  if (!error) return null;

  const headline = errorHeadline(error);
  const trace = errorDetail(error, headline);

  const handleDownload = (): void => {
    if (!trace) return;
    triggerBlobDownload(new Blob([trace], { type: 'text/plain' }), `error-trace-${Date.now()}.txt`);
  };

  const handleCopyTrace = (): void => {
    if (!trace) return;
    void handleCopy(trace);
  };

  return (
    <Box
      data-testid="error-trace"
      sx={{
        mt: 1,
        mb: 1,
        border: '1px solid',
        borderColor: 'error.main',
        borderRadius: 1,
        overflow: 'hidden',
      }}
    >
      <Box
        sx={{
          display: 'flex',
          alignItems: 'center',
          gap: 1,
          px: 1.5,
          py: 0.75,
          backgroundColor: 'error.lighter',
        }}
      >
        <ErrorOutlinedIcon
          color="error"
          fontSize="small"
        />
        <Typography
          variant="bodyMedium"
          component="p"
          color="error.main"
          sx={{ flex: 1 }}
        >
          {headline}
        </Typography>
      </Box>
      {trace && (
        <Box>
          <Box
            sx={{
              display: 'flex',
              alignItems: 'center',
              gap: 1,
              px: 1.5,
              py: 0.75,
              cursor: 'pointer',
            }}
            onClick={() => setExpanded((prev) => !prev)}
          >
            <Typography
              variant="bodyMedium"
              component="p"
              sx={{ flex: 1, color: 'text.secondary' }}
            >
              {/* eslint-disable-next-line i18next/no-literal-string — collapsible section label */}
              Error debugging info
            </Typography>
            <IconButton
              size="small"
              sx={{ transform: expanded ? 'rotate(180deg)' : 'none', transition: 'transform 0.2s' }}
            >
              ▾
            </IconButton>
          </Box>
          <Collapse in={expanded}>
            <Box sx={{ px: 1.5, pb: 1.5 }}>
              <Box sx={{ display: 'flex', justifyContent: 'flex-end', gap: 0.5, mb: 1 }}>
                {/* eslint-disable-next-line i18next/no-literal-string — tooltip label */}
                <Tooltip title="Download error trace" placement="top">
                  <IconButton
                    size="small"
                    // eslint-disable-next-line i18next/no-literal-string — accessible name
                    aria-label="Download error trace"
                    onClick={handleDownload}
                  >
                    <DownloadOutlinedIcon fontSize="small" />
                  </IconButton>
                </Tooltip>
                {/* eslint-disable-next-line i18next/no-literal-string — tooltip label */}
                <Tooltip title="Copy to clipboard" placement="top">
                  <IconButton
                    size="small"
                    // eslint-disable-next-line i18next/no-literal-string — accessible name
                    aria-label="Copy to clipboard"
                    onClick={handleCopyTrace}
                  >
                    <ContentCopyIcon fontSize="small" />
                  </IconButton>
                </Tooltip>
              </Box>
              <Box
                component="pre"
                sx={{
                  fontFamily: (theme: Theme) => theme.typography.fontFamilyMono,
                  fontSize: (theme: Theme) => theme.typography.bodySmall.fontSize,
                  color: 'error.main',
                  whiteSpace: 'pre-wrap',
                  wordBreak: 'break-word',
                  m: 0,
                }}
              >
                {trace}
              </Box>
            </Box>
          </Collapse>
        </Box>
      )}
    </Box>
  );
}
