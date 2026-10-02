import type { SxProps, Theme } from '@mui/material/styles';

export const spinnerContainerSx: SxProps<Theme> = { height: '100%', width: '100%', display: 'flex', justifyContent: 'center', alignItems: 'center' };

export const gridContainerSx: SxProps<Theme> = { height: '100%', maxHeight: '100%', paddingTop: '1rem', paddingBottom: '1.5rem', paddingLeft: '1.5rem', paddingRight: '1.5rem' };

export const leftGridItemSx: SxProps<Theme> = { overflow: 'auto', maxHeight: '100%', height: '100%' };

export const rightGridItemSx: SxProps<Theme> = { height: '100%', maxHeight: '100%' };

export const historyButtonRowSx: SxProps<Theme> = { display: 'flex', justifyContent: 'flex-end', width: '100%' };
