// RED (R-T11-STOCK): a member read of a MUI stock variant (an alias, not a role).
// A stand-in for the app theme: the rule is syntactic, so only the shape matters.
declare const theme: {
  typography: Record<string, { fontSize: string }> & { pxToRem: (px: number) => string };
};
export const sx = { fontSize: theme.typography.body2.fontSize };
