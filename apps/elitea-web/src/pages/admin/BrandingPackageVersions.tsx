/**
 * "Previous packages" (ADR-0024 WP9): the branding packages the server kept
 * — the last five applied imports, newest first — each with a Restore that
 * re-applies it exactly as an import would.
 */
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import LinearProgress from '@mui/material/LinearProgress';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';

import type { BrandingPackageVersion } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { formatPackageSize, formatPackageTime, shortDigest } from './brandingPackage';

export interface BrandingPackageVersionsProps {
  readonly versions: readonly BrandingPackageVersion[];
  readonly isLoading: boolean;
  readonly error: string | undefined;
  readonly restoringDigest: string | undefined;
  readonly disabled: boolean;
  readonly onRestore: (version: BrandingPackageVersion) => void;
}

function VersionsTable({
  versions,
  restoringDigest,
  disabled,
  onRestore,
}: Omit<BrandingPackageVersionsProps, 'isLoading' | 'error'>) {
  return (
    <Box sx={{ overflowX: 'auto' }}>
      <Table size="small" aria-label={t('pages.admin.branding.package.versions.table', 'Kept branding packages')}>
        <TableHead>
          <TableRow>
            <TableCell>{t('pages.admin.branding.package.versions.column.product', 'Product')}</TableCell>
            <TableCell>{t('pages.admin.branding.package.versions.column.exportedAt', 'Exported')}</TableCell>
            <TableCell>{t('pages.admin.branding.package.versions.column.storedAt', 'Stored')}</TableCell>
            <TableCell>{t('pages.admin.branding.package.versions.column.size', 'Size')}</TableCell>
            <TableCell>{t('pages.admin.branding.package.versions.column.digest', 'Digest')}</TableCell>
            <TableCell align="right">{t('pages.admin.branding.package.versions.column.actions', 'Actions')}</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {versions.map((version) => {
            const restoring = restoringDigest === version.digest;
            return (
              <TableRow key={version.digest} data-testid={`branding-package-version-${shortDigest(version.digest)}`}>
                <TableCell component="th" scope="row">
                  {version.product === undefined || version.product === ''
                    ? t('pages.admin.branding.package.versions.unnamed', 'Unnamed')
                    : version.product}
                </TableCell>
                <TableCell>{formatPackageTime(version.exported_at)}</TableCell>
                <TableCell>{formatPackageTime(version.stored_at)}</TableCell>
                <TableCell>{formatPackageSize(version.size)}</TableCell>
                <TableCell sx={{ fontFamily: (theme) => theme.typography.fontFamily }} title={version.digest}>
                  {shortDigest(version.digest)}
                </TableCell>
                <TableCell align="right">
                  <Button
                    size="small"
                    variant="secondary"
                    disabled={disabled || restoringDigest !== undefined}
                    onClick={() => onRestore(version)}
                    data-testid={`branding-package-restore-${shortDigest(version.digest)}`}
                  >
                    {restoring
                      ? t('pages.admin.branding.package.versions.restoring', 'Restoring…')
                      : t('pages.admin.branding.package.versions.restore', 'Restore')}
                  </Button>
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </Box>
  );
}

export function BrandingPackageVersions(props: BrandingPackageVersionsProps) {
  const { versions, isLoading, error } = props;
  return (
    <Box component="section" aria-labelledby="branding-package-versions-heading" data-testid="branding-package-versions" sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
      <Typography id="branding-package-versions-heading" variant="headingSmall" component="h2">
        {t('pages.admin.branding.package.versions.title', 'Previous packages')}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.branding.package.versions.description',
          'Every applied import is kept for rollback; the last five stay. Restore re-applies one exactly as an import would.',
        )}
      </Typography>
      {isLoading ? (
        <LinearProgress aria-label={t('pages.admin.branding.package.versions.loading', 'Loading kept packages')} />
      ) : error !== undefined ? (
        <Alert severity="warning" data-testid="branding-package-versions-error">
          {error}
        </Alert>
      ) : versions.length === 0 ? (
        <Typography variant="bodySmall" color="text.secondary" data-testid="branding-package-versions-empty">
          {t('pages.admin.branding.package.versions.empty', 'No packages have been applied yet.')}
        </Typography>
      ) : (
        <VersionsTable {...props} />
      )}
    </Box>
  );
}
