import { Fragment, memo, useCallback, type ReactNode, type UIEvent } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import ListItemButton from '@mui/material/ListItemButton';
import Popover from '@mui/material/Popover';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn, BUTTON_VARIANTS } from '@/shared/ui/BaseBtn';
import { PlusIcon } from '@/shared/ui/icons/plus-icon';
import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';

import type { PickerCreateAction, PickerGroup, PickerItem } from '../../lib/tool-picker/toolPicker.types';

/** Distance from the list bottom (in px) that triggers loading the next page. */
const LOAD_MORE_THRESHOLD = 48;

export interface PickerMenuMessages {
  readonly searchPlaceholder?: string | undefined;
  readonly emptyMessage?: string | undefined;
  readonly noResultsMessage?: string | undefined;
}

export interface PickerMenuLoadState {
  readonly isLoading?: boolean | undefined;
  readonly isError?: boolean | undefined;
  readonly onRetry?: (() => void) | undefined;
  readonly onLoadMore?: (() => void) | undefined;
}

export interface PickerMenuProps<TItem extends PickerItem = PickerItem> {
  readonly anchorEl: HTMLElement | null;
  readonly open: boolean;
  readonly onClose: () => void;
  readonly searchValue: string;
  readonly onSearchChange: (value: string) => void;
  readonly createActions?: readonly PickerCreateAction[] | undefined;
  readonly groups?: readonly PickerGroup<TItem>[] | undefined;
  readonly renderItem: (item: TItem) => ReactNode;
  readonly loadState?: PickerMenuLoadState | undefined;
  readonly messages?: PickerMenuMessages | undefined;
  readonly 'data-testid'?: string | undefined;
}

function buildPaperSx(anchorWidth: number | undefined): SxProps<Theme> {
  return (theme: Theme) => ({
    width: anchorWidth ? `max(${String(anchorWidth / 16)}rem, 20rem)` : '20rem',
    maxHeight: '26rem',
    marginTop: theme.spacing(0.5),
    display: 'flex',
    flexDirection: 'column',
    borderRadius: theme.vars.shape.radiusMd,
    border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
    backgroundColor: theme.vars.palette.background.paper,
  });
}

const searchSx: SxProps<Theme> = (theme: Theme) => ({
  flexShrink: 0,
  borderRadius: theme.vars.shape.radiusSm,
  border: 'none',
  borderBottom: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  backgroundColor: 'transparent',
  paddingBlock: theme.spacing(1.5),
  paddingInline: theme.spacing(2),
});

const createActionSx: SxProps<Theme> = (theme: Theme) => ({
  gap: theme.spacing(1.5),
  paddingBlock: theme.spacing(1),
  paddingInline: theme.spacing(2),
  borderBottom: `0.0625rem solid ${theme.vars.palette.border.lines}`,
});

const plusIconSx: SxProps<Theme> = {
  width: '1rem',
  height: '1rem',
};

const listSx: SxProps<Theme> = (theme: Theme) => ({
  flex: 1,
  minHeight: 0,
  overflowY: 'auto',
  paddingBlock: theme.spacing(0.5),
});

const groupTitleSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'block',
  paddingTop: theme.spacing(1),
  paddingBottom: theme.spacing(0.5),
  paddingInline: theme.spacing(2),
  color: theme.vars.palette.text.metrics,
  textTransform: 'uppercase',
});

const messageSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  gap: theme.spacing(1),
  padding: theme.spacing(2),
  textAlign: 'center',
});

function PickerMenuListBody<TItem extends PickerItem>({
  groups,
  renderItem,
  isLoading,
  isError,
  onRetry,
  emptyText,
  testId,
}: {
  readonly groups: readonly PickerGroup<TItem>[];
  readonly renderItem: (item: TItem) => ReactNode;
  readonly isLoading: boolean;
  readonly isError: boolean;
  readonly onRetry: (() => void) | undefined;
  readonly emptyText: string;
  readonly testId: string | undefined;
}): ReactNode {
  if (isError) {
    return (
      <Box sx={messageSx} data-testid={testId ? `${testId}-error` : undefined}>
        <Typography variant="bodySmall" color="text.primary">
          {t('pipelines.toolPicker.loadError', 'Could not load the list.')}
        </Typography>
        <BaseBtn
          variant={BUTTON_VARIANTS.tertiary}
          onClick={onRetry}
          data-testid={testId ? `${testId}-retry` : undefined}
        >
          {t('pipelines.toolPicker.retry', 'Retry')}
        </BaseBtn>
      </Box>
    );
  }

  const itemsCount = groups.reduce((count, group) => count + group.items.length, 0);
  const showGroupTitles = groups.filter((group) => group.items.length > 0).length > 1;

  return (
    <>
      {groups.map((group) => (
        <Fragment key={group.key}>
          {showGroupTitles && group.items.length > 0 ? (
            <Typography variant="labelSmall" sx={groupTitleSx}>
              {group.title}
            </Typography>
          ) : null}
          {group.items.map(renderItem)}
        </Fragment>
      ))}
      {isLoading ? (
        <Box sx={messageSx}>
          <CircularProgress size="1.25rem" />
        </Box>
      ) : null}
      {!isLoading && itemsCount === 0 ? (
        <Box sx={messageSx} data-testid={testId ? `${testId}-empty` : undefined}>
          <Typography variant="bodySmall" color="text.primary">
            {emptyText}
          </Typography>
        </Box>
      ) : null}
    </>
  );
}

function resolveMenuMessages(messages: PickerMenuMessages | undefined) {
  return {
    searchPlaceholder:
      messages?.searchPlaceholder ?? t('pipelines.toolPicker.searchPlaceholder', 'Search...'),
    emptyMessage: messages?.emptyMessage ?? t('pipelines.toolPicker.emptyMessage', 'Nothing here yet'),
    noResultsMessage:
      messages?.noResultsMessage ?? t('pipelines.toolPicker.noResultsMessage', 'Nothing found'),
  };
}

function PickerMenuInner<TItem extends PickerItem = PickerItem>({
  anchorEl,
  open,
  onClose,
  searchValue,
  onSearchChange,
  createActions = [],
  groups = [],
  renderItem,
  loadState,
  messages,
  'data-testid': testId,
}: PickerMenuProps<TItem>): ReactNode {
  const isLoading = Boolean(loadState?.isLoading);
  const isError = Boolean(loadState?.isError);
  const onLoadMore = loadState?.onLoadMore;
  const resolved = resolveMenuMessages(messages);

  const handleScroll = useCallback(
    (event: UIEvent<HTMLDivElement>) => {
      if (isLoading || isError) return;
      const { scrollTop, scrollHeight, clientHeight } = event.currentTarget;
      if (scrollTop + clientHeight >= scrollHeight - LOAD_MORE_THRESHOLD) {
        onLoadMore?.();
      }
    },
    [isError, isLoading, onLoadMore],
  );

  const handleSearchClear = useCallback(() => {
    onSearchChange('');
  }, [onSearchChange]);

  const paperSlotProps = {
    sx: buildPaperSx(anchorEl?.clientWidth),
    ...(testId ? { 'data-testid': testId } : {}),
  };
  const searchTestIdProp = testId ? { 'data-testid': `${testId}-search` } : {};

  return (
    <Popover
      className="nokey"
      open={open}
      anchorEl={anchorEl}
      onClose={onClose}
      anchorOrigin={{ vertical: 'bottom', horizontal: 'left' }}
      transformOrigin={{ vertical: 'top', horizontal: 'left' }}
      slotProps={{ paper: paperSlotProps }}
    >
      <SimpleSearchBar
        value={searchValue}
        onChange={onSearchChange}
        onClear={handleSearchClear}
        placeholder={resolved.searchPlaceholder}
        debounceMs={0}
        sx={searchSx}
        aria-label={resolved.searchPlaceholder}
        {...searchTestIdProp}
      />
      {createActions.map((action) => (
        <ListItemButton
          key={action.key}
          onClick={action.onClick}
          sx={createActionSx}
          data-testid={action.testId}
        >
          <Box component={PlusIcon} sx={plusIconSx} />
          <Typography variant="bodyMedium" color="text.secondary">
            {action.label}
          </Typography>
        </ListItemButton>
      ))}
      <Box sx={listSx} onScroll={handleScroll}>
        <PickerMenuListBody
          groups={groups}
          renderItem={renderItem}
          isLoading={isLoading}
          isError={isError}
          onRetry={loadState?.onRetry}
          emptyText={searchValue ? resolved.noResultsMessage : resolved.emptyMessage}
          testId={testId}
        />
      </Box>
    </Popover>
  );
}

/** Searchable, scroll-loading menu of a picker: search, create actions, grouped items, loading/empty/error states. */
export const PickerMenu = memo(PickerMenuInner) as typeof PickerMenuInner;
