import { useState, type ReactNode, type SyntheticEvent } from 'react';

import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlineOutlined';
import DownloadOutlinedIcon from '@mui/icons-material/DownloadOutlined';
import LocalOfferOutlinedIcon from '@mui/icons-material/LocalOfferOutlined';
import Box from '@mui/material/Box';
import InputBase from '@mui/material/InputBase';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

/**
 * The List tab's selection section (ADR-0051): shown while rows are selected,
 * above the page's own controls. A page passes only the actions its rows
 * support — Add tag, Export, Delete — and `extra` for anything it adds on
 * top (the project folders' Move to folder, ADR-0051 M2, goes here).
 */
export interface ListSelectionSectionProps {
  readonly count: number;
  /** Lower-case, plural: "agents". */
  readonly noun: string;
  readonly onClear: () => void;
  /** Adds one tag to every selected row. Absent where the rows carry no tags. */
  readonly onAddTag?: ((tag: string) => void) | undefined;
  readonly onExport?: (() => void) | undefined;
  readonly onDelete?: (() => void) | undefined;
  /** Extra actions the page adds after the three above. */
  readonly extra?: ReactNode;
  /** An action is running: the buttons wait. */
  readonly busy?: boolean;
  /** The last action's refusal, said as text. */
  readonly error?: string | undefined;
  readonly testIdPrefix: string;
}

export function ListSelectionSection({ count, noun, onClear, onAddTag, onExport, onDelete, extra, busy = false, error, testIdPrefix }: ListSelectionSectionProps): ReactNode {
  const [tagging, setTagging] = useState(false);
  const [tag, setTag] = useState('');

  const submitTag = (event: SyntheticEvent): void => {
    event.preventDefault();
    const name = tag.trim();
    if (name === '' || onAddTag === undefined) return;
    onAddTag(name);
    setTag('');
    setTagging(false);
  };

  return (
    <Box
      component="section"
      aria-label={t('widgets.listSidebar.selection.label', 'Selection')}
      data-testid={`${testIdPrefix}-selection`}
      sx={sectionSx}
    >
      <Box sx={headerSx}>
        <Typography
          variant="labelMedium"
          component="output"
          data-testid={`${testIdPrefix}-selection-count`}
        >
          {t('widgets.listSidebar.selection.count', '{{count}} {{noun}} selected', { count, noun })}
        </Typography>
        <BaseBtn
          variant="text"
          onClick={onClear}
          data-testid={`${testIdPrefix}-selection-clear`}
        >
          {t('widgets.listSidebar.selection.clear', 'Clear selection')}
        </BaseBtn>
      </Box>
      <Box sx={actionsSx}>
        {onAddTag !== undefined && (
          <BaseBtn
            variant="secondary"
            startIcon={<LocalOfferOutlinedIcon />}
            disabled={busy}
            aria-expanded={tagging}
            onClick={() => setTagging((open) => !open)}
            data-testid={`${testIdPrefix}-selection-add-tag`}
          >
            {t('widgets.listSidebar.selection.addTag', 'Add tag')}
          </BaseBtn>
        )}
        {onExport !== undefined && (
          <BaseBtn
            variant="secondary"
            startIcon={<DownloadOutlinedIcon />}
            disabled={busy}
            onClick={onExport}
            data-testid={`${testIdPrefix}-selection-export`}
          >
            {t('widgets.listSidebar.selection.export', 'Export')}
          </BaseBtn>
        )}
        {extra}
        {onDelete !== undefined && (
          <BaseBtn
            variant="alarm"
            startIcon={<DeleteOutlineIcon />}
            disabled={busy}
            onClick={onDelete}
            data-testid={`${testIdPrefix}-selection-delete`}
          >
            {t('widgets.listSidebar.selection.delete', 'Delete')}
          </BaseBtn>
        )}
      </Box>
      {tagging && onAddTag !== undefined && (
        <Box
          component="form"
          onSubmit={submitTag}
          sx={tagFormSx}
        >
          <InputBase
            inputRef={(node: HTMLInputElement | null) => node?.focus()}
            value={tag}
            onChange={(event) => setTag(event.target.value)}
            placeholder={t('widgets.listSidebar.selection.tagPlaceholder', 'Tag name')}
            inputProps={{ 'aria-label': t('widgets.listSidebar.selection.tagLabel', 'Tag to add'), 'data-testid': `${testIdPrefix}-selection-tag-input`, maxLength: 64 }}
            sx={tagInputSx}
          />
          <BaseBtn
            type="submit"
            variant="contained"
            disabled={busy || tag.trim() === ''}
            data-testid={`${testIdPrefix}-selection-tag-apply`}
          >
            {t('widgets.listSidebar.selection.apply', 'Apply')}
          </BaseBtn>
        </Box>
      )}
      {error !== undefined && (
        <Typography
          variant="bodySmall"
          role="alert"
          sx={errorSx}
        >
          {error}
        </Typography>
      )}
    </Box>
  );
}

const sectionSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(1),
  padding: theme.spacing(1.5),
  borderRadius: theme.vars.shape.radiusLg,
  border: `1px solid ${theme.vars.palette.border.flowNode}`,
});

const headerSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 1 };

const actionsSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: 1 };

const tagFormSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: 1 };

const tagInputSx: SxProps<Theme> = (theme: Theme) => ({
  flex: 1,
  minWidth: 0,
  paddingInline: theme.spacing(1.5),
  paddingBlock: theme.spacing(0.5),
  borderRadius: theme.vars.shape.radiusLg,
  border: `1px solid ${theme.vars.palette.border.lines}`,
  backgroundColor: theme.vars.palette.background.userInputBackground,
});

const errorSx: SxProps<Theme> = (theme: Theme) => ({ color: theme.vars.palette.error.main });
