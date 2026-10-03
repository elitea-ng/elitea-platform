// GREEN: proves `oxlint-disable-next-line elitea/ad-hoc-font-size` suppresses
// through the jsPlugins bridge (the repo's older comments say per-line
// disables do not pass through it — true for eslint-plugin-i18next, not
// for the local elitea plugin).
export const sx = {
  // oxlint-disable-next-line elitea/ad-hoc-font-size
  fontSize: '0.8rem',
};
