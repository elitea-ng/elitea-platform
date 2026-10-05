// GREEN (R-T11-STOCK).
// A stand-in for the app theme: the rule is syntactic, so only the shape matters.
declare const theme: {
  typography: Record<string, { fontSize: string }> & { pxToRem: (px: number) => string };
};

// Every allowed shape: the keyword, a custom variant's size, a conditional
// between two custom variants, and an sx per-property callback.
export const allowedSx = {
  inherit: { fontSize: 'inherit' },
  member: { fontSize: theme.typography.bodySmall.fontSize },
  conditional: (dense: boolean) => ({
    fontSize: dense ? theme.typography.bodySmall.fontSize : theme.typography.bodyMedium.fontSize,
  }),
  callback: { fontSize: (t: typeof theme) => t.typography.headingSmall.fontSize },
};
