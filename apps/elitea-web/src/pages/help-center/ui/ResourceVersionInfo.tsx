/**
 * ResourceVersionInfo — version info bar at the top of the Help Center.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/pages/resources/ui/ResourceVersionInfo.jsx`.
 *
 * The baseline derives `versionLabel` and `plugins` internally from
 * `configValues`/`systemInfo` API responses; this component instead takes
 * the already-derived `versionLabel` string and a `components` list as props,
 * so it stays a pure presentational component regardless of where that data
 * comes from. `HelpCenterPage` sources both from `../lib/useResourcesConfig`
 * — see that hook's module doc for what `components` reports now that issue
 * #892 gave `GET /admin/system_info/prompt_lib` a real 200 (this service's own
 * build version, plus the migration head when a database answered).
 */
import { memo, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import type { SystemStyleObject } from '@mui/system';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { CopyToClipboardButton } from '@/shared/ui/CopyToClipboardButton';
import { InfoIcon } from '@/shared/ui/icons/info-icon';

import type { ResourcesConfigComponent } from '../lib/useResourcesConfig';

/** Props consumed by ResourceVersionInfo. */
interface ResourceVersionInfoProps {
  /** Pre-formatted "Version: X (date)" label. Defaults to the empty string (bar hidden). */
  versionLabel?: string;
  /** This service's own known component versions, shown in the info-icon tooltip. Defaults to none. */
  components?: ReadonlyArray<ResourcesConfigComponent>;
}

/** Theme-aware sx values for the version info header. */
const headerSx: SxProps<Theme> = (t: Theme): SystemStyleObject<Theme> => ({
  flexShrink: 0,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  px: t.spacing(3),
  height: '3.75rem',
  minHeight: '3.75rem',
  borderBottom: `0.0625rem solid ${t.palette.divider}`,
  backgroundColor: t.vars.palette.background?.tabPanel ?? t.palette.background.default,
});

const headerRightSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
};

const headerVersionInfoSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  gap: '0.375rem',
};

const infoIconSx: SxProps<Theme> = {
  display: 'inline-flex',
  alignItems: 'center',
  flexShrink: 0,
  cursor: 'pointer',
  color: (t: Theme) => t.palette.text.primary,
};

const infoIconSvgSx: SxProps<Theme> = {
  width: '0.875rem',
  height: '0.875rem',
  display: 'block',
};

const tooltipContentSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.25rem',
  padding: '0.125rem',
};

const tooltipRowSx: SxProps<Theme> = {
  display: 'flex',
  gap: '0.25rem',
  alignItems: 'baseline',
};

/** Placeholder for a component with no reported version — matches the baseline's `'—'` fallback. */
const NO_VERSION_PLACEHOLDER = '—';

/**
 * Header bar showing the "Help Center" title and optional version info.
 */
const ResourceVersionInfo = memo(({ versionLabel = '', components = [] }: ResourceVersionInfoProps): ReactNode => {
  const versionInfoText = versionLabel || '';
  const hasVersion = versionInfoText.length > 0;

  return (
    <Box sx={headerSx}>
      <Typography
        variant="headingMedium"
        color="text.secondary"
      >
        Help Center
      </Typography>
      <Box sx={headerRightSx}>
        {hasVersion && (
          <Box sx={headerVersionInfoSx}>
            <Typography
              variant="bodyMedium"
              color="text.secondary"
            >
              {versionInfoText}
            </Typography>
            <Tooltip
              placement="bottom-end"
              title={
                components.length > 0 ? (
                  <Box sx={tooltipContentSx}>
                    {components.map(component => (
                      <Box
                        key={component.name}
                        sx={tooltipRowSx}
                      >
                        <Typography variant="bodySmall">
                          {component.name}: {component.version || NO_VERSION_PLACEHOLDER}
                        </Typography>
                      </Box>
                    ))}
                  </Box>
                ) : null
              }
            >
              <Box
                sx={infoIconSx}
                data-testid="resource-version-info-icon"
              >
                <Box
                  component={InfoIcon}
                  sx={infoIconSvgSx}
                />
              </Box>
            </Tooltip>
            <CopyToClipboardButton
              label=""
              value={versionInfoText}
              data-testid="copy-version-info"
            />
          </Box>
        )}
      </Box>
    </Box>
  );
});

ResourceVersionInfo.displayName = 'ResourceVersionInfo';

export default ResourceVersionInfo;
