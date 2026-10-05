/**
 * `elitea` — the local oxlint JS plugin hosting the custom fence rules from
 * spec §3.4 as remapped by decision D2 (oxlint 1.75 has no
 * no-restricted-syntax, so every syntax-shaped fence is a real rule here).
 *
 * Written against the standard ESLint rule API; loaded via `jsPlugins` in
 * .oxlintrc.json. Every rule ships a failing and a passing fixture under
 * tools/lint-rules/fixtures/, proven by scripts/check-gates-selftest.mjs —
 * a rule whose failing fixture passes is a defect (D2 acceptance).
 */
import { adHocFontSize } from './rules/ad-hoc-font-size.mjs';
import { adHocRadius } from './rules/ad-hoc-radius.mjs';
import { noAdHocEnvelopeUnwrap } from './rules/no-adhoc-envelope-unwrap.mjs';
import { noExportAll } from './rules/no-export-all.mjs';
import { noImportantSx } from './rules/no-important-sx.mjs';
import { noModeBranch } from './rules/no-mode-branch.mjs';
import { noModuleScopeStore } from './rules/no-module-scope-store.mjs';
import { noMuiInternalSelector } from './rules/no-mui-internal-selector.mjs';
import { noRawColor } from './rules/no-raw-color.mjs';
import { noRawWebstorage } from './rules/no-raw-webstorage.mjs';
import { noStockTypographyVariant } from './rules/no-stock-typography-variant.mjs';
import { noThemePalette } from './rules/no-theme-palette.mjs';
import { noViMock } from './rules/no-vi-mock.mjs';
import { rawPxSpacing } from './rules/raw-px-spacing.mjs';

export default {
  meta: {
    name: 'elitea',
    version: '1.0.0',
  },
  rules: {
    'no-raw-color': noRawColor, //             R-T1
    'no-mode-branch': noModeBranch, //         R-T2 (theme-gate grep is the backstop)
    'no-important-sx': noImportantSx, //       R-T5
    'no-mui-internal-selector': noMuiInternalSelector, // R-T6
    'no-theme-palette': noThemePalette, //     R-T7
    'raw-px-spacing': rawPxSpacing, //         R-T9
    'ad-hoc-radius': adHocRadius, //           R-T10
    'ad-hoc-font-size': adHocFontSize, //      R-T11
    'no-stock-typography-variant': noStockTypographyVariant, // R-T13
    'no-export-all': noExportAll, //           R-L4
    'no-module-scope-store': noModuleScopeStore, // R-S2
    'no-vi-mock': noViMock, //                 R-M1
    'no-raw-webstorage': noRawWebstorage, //   STOR-1 (member-expression half, issue #22)
    'no-adhoc-envelope-unwrap': noAdHocEnvelopeUnwrap, // R-A6 (issue #132)
  },
};
