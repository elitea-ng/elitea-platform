import type { ReactNode } from 'react';
import { useMemo } from 'react';

import Box from '@mui/material/Box';
import Paper from '@mui/material/Paper';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';

import Button from '@mui/material/Button';

import { t } from '@/shared/i18n';
import { CodeMirrorEditor } from '@/shared/ui/CodeMirrorEditor';
import { DefaultMarkdown } from '@/shared/ui/DefaultMarkdown';

import { parseDataFile, type ArtifactPreviewKind } from '../lib/artifactParsers';
import { PdfPreview } from './PdfPreview';

interface ArtifactPreviewContentProps {
  readonly kind: ArtifactPreviewKind;
  readonly filename: string;
  readonly content: string;
  readonly imageUrl?: string;
  /** The fetched bytes of a `pdf` kind file. */
  readonly pdfData?: Blob;
  readonly mode: 'code' | 'rendered';
  readonly onChange: (content: string) => void;
  readonly onDownload: () => void;
  /** File exceeds ARTIFACT_PREVIEW_SIZE_LIMIT_BYTES — content was never fetched. */
  readonly isOversized?: boolean;
}

function DataPreview({ content, kind }: { readonly content: string; readonly kind: 'csv' | 'tsv' }): ReactNode {
  const data = useMemo(() => parseDataFile(content, kind), [content, kind]);
  if (data.headers.length === 0) {
    return <Typography>{t('artifacts.preview.noData', 'No data to display.')}</Typography>;
  }
  return (
    <TableContainer
      component={Paper}
      sx={{ maxHeight: '100%' }}
    >
      <Table
        size="small"
        stickyHeader
      >
        <TableHead>
          <TableRow>
            {data.headers.map((header, index) => <TableCell key={`${header}-${index}`}>{header || `Column ${index + 1}`}</TableCell>)}
          </TableRow>
        </TableHead>
        <TableBody>
          {data.rows.map((row, rowIndex) => (
            <TableRow key={rowIndex}>
              {data.headers.map((_header, cellIndex) => <TableCell key={cellIndex}>{row[cellIndex] ?? ''}</TableCell>)}
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  );
}

// oxlint-disable-next-line complexity -- dispatches the intentionally distinct preview formats documented by A8.
export function ArtifactPreviewContent(props: ArtifactPreviewContentProps): ReactNode {
  if (props.isOversized || props.kind === 'docx' || props.kind === 'unsupported') {
    return (
      <Box sx={{ display: 'grid', placeItems: 'center', minHeight: '16rem', textAlign: 'center' }}>
        <Box>
          <Typography variant="headingSmall">
            {t('artifacts.preview.unavailableTitle', 'Preview unavailable')}
          </Typography>
          <Typography
            variant="bodySmall"
            sx={{ mt: 1, mb: 2 }}
          >
            {props.isOversized
              ? t('artifacts.preview.tooLarge', 'This file is too large to preview. Download it to view the contents.')
              : props.kind === 'docx'
                ? t(
                  'artifacts.preview.docxUnavailable',
                  'DOCX editing is not available in this release. Download the file to open it.',
                )
                : t('artifacts.preview.typeUnavailable', 'This file type cannot be previewed.')}
          </Typography>
          <Button
            variant="contained"
            onClick={props.onDownload}
          >
            {t('artifacts.preview.downloadFile', 'Download file')}
          </Button>
        </Box>
      </Box>
    );
  }
  if (props.kind === 'image') {
    return props.imageUrl === undefined ? (
      <Typography>{t('artifacts.preview.noImage', 'No image to display.')}</Typography>
    ) : (
      <Box
        component="img"
        src={props.imageUrl}
        alt={props.filename}
        sx={{ maxWidth: '100%', maxHeight: '100%', objectFit: 'contain' }}
      />
    );
  }
  if (props.kind === 'pdf') {
    return props.pdfData === undefined ? (
      <Typography>{t('artifacts.preview.noPdf', 'No PDF to display.')}</Typography>
    ) : (
      <PdfPreview
        data={props.pdfData}
        filename={props.filename}
        onDownload={props.onDownload}
      />
    );
  }
  if (props.mode === 'rendered' && props.kind === 'markdown') {
    return <DefaultMarkdown markdown={props.content} />;
  }
  if (props.mode === 'rendered' && (props.kind === 'csv' || props.kind === 'tsv')) {
    return <DataPreview content={props.content} kind={props.kind} />;
  }
  if (props.mode === 'rendered' && props.kind === 'mermaid') {
    return (
      <Box>
        <Typography
          variant="bodySmall"
          sx={{ mb: 1 }}
        >
          {t(
            'artifacts.preview.mermaidRaw',
            'Live Mermaid rendering is not enabled. The diagram definition is shown below.',
          )}
        </Typography>
        <Box
          component="pre"
          sx={{ m: 0, whiteSpace: 'pre-wrap' }}
        >
          {props.content}
        </Box>
      </Box>
    );
  }
  return (
    <CodeMirrorEditor
      value={props.content}
      onChange={props.onChange}
      height="100%"
      minHeight="24rem"
      aria-label={`Edit ${props.filename}`}
    />
  );
}
