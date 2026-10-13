//! Render-side theme color resolver.
//!
//! Given a deck's color scheme (from `theme1.xml` `<a:clrScheme>`) and a
//! `SchemeColor` reference, resolves to an RGB [`Color`]. This is the render-side
//! counterpart to the oxml emitter: the emitter writes `<a:schemeClr val="..."/>`
//! and the resolver reads the theme to produce the concrete color for rendering.
//!
//! Also provides:
//! - Color modifiers: tint, shade, lumMod, lumOff (ECMA-376 §20.1.2.3)
//! - Theme font resolution: `+mn-lt` / `+mj-lt` → actual typeface names
//! - Color map: `clrMap` / `clrMapOvr` (slide-level color map overrides)
//!
//! # Usage
//!
//! ```
//! use zavora_slide_layout::Color;
//! use zavora_slide_oxml::SchemeColor;
//! use zavora_slide_render::ThemeColorScheme;
//!
//! let scheme = ThemeColorScheme::office_default();
//! assert_eq!(scheme.resolve(SchemeColor::Dk1), Color::BLACK);
//! assert_eq!(scheme.resolve(SchemeColor::Lt1), Color::WHITE);
//! ```

use zavora_slide_layout::Color;
use zavora_slide_oxml::SchemeColor;

// ─── HSL helpers (internal) ─────────────────────────────────────────────────

/// Convert sRGB (0–255 per channel) to HSL (h in 0..360, s/l in 0.0..1.0).
fn rgb_to_hsl(r: u8, g: u8, b: u8) -> (f64, f64, f64) {
    let rf = r as f64 / 255.0;
    let gf = g as f64 / 255.0;
    let bf = b as f64 / 255.0;
    let max = rf.max(gf).max(bf);
    let min = rf.min(gf).min(bf);
    let l = (max + min) / 2.0;
    if (max - min).abs() < 1e-10 {
        return (0.0, 0.0, l);
    }
    let d = max - min;
    let s = if l > 0.5 {
        d / (2.0 - max - min)
    } else {
        d / (max + min)
    };
    let h = if (max - rf).abs() < 1e-10 {
        let mut h = (gf - bf) / d;
        if gf < bf {
            h += 6.0;
        }
        h
    } else if (max - gf).abs() < 1e-10 {
        (bf - rf) / d + 2.0
    } else {
        (rf - gf) / d + 4.0
    };
    (h * 60.0, s, l)
}

/// Convert HSL (h in 0..360, s/l in 0.0..1.0) back to sRGB (0–255).
fn hsl_to_rgb(h: f64, s: f64, l: f64) -> (u8, u8, u8) {
    if s.abs() < 1e-10 {
        let v = (l * 255.0).round().clamp(0.0, 255.0) as u8;
        return (v, v, v);
    }
    let q = if l < 0.5 {
        l * (1.0 + s)
    } else {
        l + s - l * s
    };
    let p = 2.0 * l - q;
    let hn = h / 360.0;
    let r = hue_to_channel(p, q, hn + 1.0 / 3.0);
    let g = hue_to_channel(p, q, hn);
    let b = hue_to_channel(p, q, hn - 1.0 / 3.0);
    (
        (r * 255.0).round().clamp(0.0, 255.0) as u8,
        (g * 255.0).round().clamp(0.0, 255.0) as u8,
        (b * 255.0).round().clamp(0.0, 255.0) as u8,
    )
}

fn hue_to_channel(p: f64, q: f64, mut t: f64) -> f64 {
    if t < 0.0 {
        t += 1.0;
    }
    if t > 1.0 {
        t -= 1.0;
    }
    if t < 1.0 / 6.0 {
        p + (q - p) * 6.0 * t
    } else if t < 0.5 {
        q
    } else if t < 2.0 / 3.0 {
        p + (q - p) * (2.0 / 3.0 - t) * 6.0
    } else {
        p
    }
}

// ─── Color Modifiers ────────────────────────────────────────────────────────

/// A color modifier that can be applied to a resolved color.
///
/// These correspond to child elements of `<a:schemeClr>` (or `<a:srgbClr>`) in
/// DrawingML, e.g. `<a:tint val="50000"/>`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ColorModifier {
    /// Tint: move color toward white. Value in thousandths (0–100000).
    Tint(u32),
    /// Shade: move color toward black. Value in thousandths (0–100000).
    Shade(u32),
    /// Luminance modulation: multiply luminance by val/100000.
    LumMod(u32),
    /// Luminance offset: add val/100000 to luminance.
    LumOff(i32),
}

/// Apply a tint modifier to a color.
///
/// Tint moves the color toward white. `tint_val` is in thousandths (0–100000),
/// where 100000 means no change and 0 means pure white.
/// Per ECMA-376: new_lum = lum * (tint_val/100000) + (1 - tint_val/100000).
pub fn apply_tint(color: Color, tint_val: u32) -> Color {
    let t = (tint_val.min(100_000) as f64) / 100_000.0;
    let (h, s, l) = rgb_to_hsl(color.r, color.g, color.b);
    let new_l = (l * t + (1.0 - t)).clamp(0.0, 1.0);
    let (r, g, b) = hsl_to_rgb(h, s, new_l);
    Color { r, g, b }
}

/// Apply a shade modifier to a color.
///
/// Shade moves the color toward black. `shade_val` is in thousandths (0–100000),
/// where 100000 means no change and 0 means pure black.
/// Per ECMA-376: new_lum = lum * (shade_val/100000).
pub fn apply_shade(color: Color, shade_val: u32) -> Color {
    let s_factor = (shade_val.min(100_000) as f64) / 100_000.0;
    let (h, s, l) = rgb_to_hsl(color.r, color.g, color.b);
    let new_l = (l * s_factor).clamp(0.0, 1.0);
    let (r, g, b) = hsl_to_rgb(h, s, new_l);
    Color { r, g, b }
}

/// Apply a luminance modulation modifier.
///
/// Multiplies the luminance by `lum_mod / 100000`. A value of 100000 means no
/// change; 50000 halves the luminance.
pub fn apply_lum_mod(color: Color, lum_mod: u32) -> Color {
    let factor = (lum_mod as f64) / 100_000.0;
    let (h, s, l) = rgb_to_hsl(color.r, color.g, color.b);
    let new_l = (l * factor).clamp(0.0, 1.0);
    let (r, g, b) = hsl_to_rgb(h, s, new_l);
    Color { r, g, b }
}

/// Apply a luminance offset modifier.
///
/// Adds `lum_off / 100000` to the luminance. Positive values lighten, negative
/// values darken.
pub fn apply_lum_off(color: Color, lum_off: i32) -> Color {
    let offset = (lum_off as f64) / 100_000.0;
    let (h, s, l) = rgb_to_hsl(color.r, color.g, color.b);
    let new_l = (l + offset).clamp(0.0, 1.0);
    let (r, g, b) = hsl_to_rgb(h, s, new_l);
    Color { r, g, b }
}

/// Apply a sequence of color modifiers to a base color.
///
/// Modifiers are applied in order (as they appear in the XML child list).
pub fn apply_modifiers(color: Color, modifiers: &[ColorModifier]) -> Color {
    let mut c = color;
    for m in modifiers {
        c = match *m {
            ColorModifier::Tint(v) => apply_tint(c, v),
            ColorModifier::Shade(v) => apply_shade(c, v),
            ColorModifier::LumMod(v) => apply_lum_mod(c, v),
            ColorModifier::LumOff(v) => apply_lum_off(c, v),
        };
    }
    c
}

// ─── ThemeColorScheme ───────────────────────────────────────────────────────

/// A resolved color scheme mapping each [`SchemeColor`] to an RGB [`Color`].
///
/// Constructed by parsing a theme's `<a:clrScheme>` element. The resolver holds
/// the 12 concrete RGB values for the scheme colors defined in the theme.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ThemeColorScheme {
    pub dk1: Color,
    pub dk2: Color,
    pub lt1: Color,
    pub lt2: Color,
    pub accent1: Color,
    pub accent2: Color,
    pub accent3: Color,
    pub accent4: Color,
    pub accent5: Color,
    pub accent6: Color,
    pub hlink: Color,
    pub fol_hlink: Color,
}

impl ThemeColorScheme {
    /// The default Office theme color scheme (matches `theme1.xml` bundled with
    /// the engine).
    pub fn office_default() -> Self {
        Self {
            dk1: Color::BLACK, // sysClr windowText → 000000
            dk2: Color::from_hex("1F497D").unwrap(),
            lt1: Color::WHITE, // sysClr window → FFFFFF
            lt2: Color::from_hex("EEECE1").unwrap(),
            accent1: Color::from_hex("4F81BD").unwrap(),
            accent2: Color::from_hex("C0504D").unwrap(),
            accent3: Color::from_hex("9BBB59").unwrap(),
            accent4: Color::from_hex("8064A2").unwrap(),
            accent5: Color::from_hex("4BACC6").unwrap(),
            accent6: Color::from_hex("F79646").unwrap(),
            hlink: Color::from_hex("0000FF").unwrap(),
            fol_hlink: Color::from_hex("800080").unwrap(),
        }
    }

    /// Construct a color scheme from individual hex color strings.
    ///
    /// Each argument is a 6-character hex string (with or without `#` prefix).
    /// Returns `None` if any color string is malformed.
    #[allow(clippy::too_many_arguments)]
    pub fn from_hex(
        dk1: &str,
        dk2: &str,
        lt1: &str,
        lt2: &str,
        accent1: &str,
        accent2: &str,
        accent3: &str,
        accent4: &str,
        accent5: &str,
        accent6: &str,
        hlink: &str,
        fol_hlink: &str,
    ) -> Option<Self> {
        Some(Self {
            dk1: Color::from_hex(dk1)?,
            dk2: Color::from_hex(dk2)?,
            lt1: Color::from_hex(lt1)?,
            lt2: Color::from_hex(lt2)?,
            accent1: Color::from_hex(accent1)?,
            accent2: Color::from_hex(accent2)?,
            accent3: Color::from_hex(accent3)?,
            accent4: Color::from_hex(accent4)?,
            accent5: Color::from_hex(accent5)?,
            accent6: Color::from_hex(accent6)?,
            hlink: Color::from_hex(hlink)?,
            fol_hlink: Color::from_hex(fol_hlink)?,
        })
    }

    /// Resolve a [`SchemeColor`] to its concrete RGB [`Color`] in this scheme.
    pub fn resolve(&self, scheme_color: SchemeColor) -> Color {
        match scheme_color {
            SchemeColor::Dk1 => self.dk1,
            SchemeColor::Dk2 => self.dk2,
            SchemeColor::Lt1 => self.lt1,
            SchemeColor::Lt2 => self.lt2,
            SchemeColor::Accent1 => self.accent1,
            SchemeColor::Accent2 => self.accent2,
            SchemeColor::Accent3 => self.accent3,
            SchemeColor::Accent4 => self.accent4,
            SchemeColor::Accent5 => self.accent5,
            SchemeColor::Accent6 => self.accent6,
            SchemeColor::Hlink => self.hlink,
            SchemeColor::FolHlink => self.fol_hlink,
        }
    }

    /// Get the color for a scheme color name string (e.g. "accent1").
    ///
    /// Returns `None` if the name is not a valid scheme color.
    pub fn resolve_by_name(&self, name: &str) -> Option<Color> {
        SchemeColor::from_val(name).map(|sc| self.resolve(sc))
    }

    /// Resolve a scheme color with modifiers applied.
    ///
    /// First resolves the scheme color to its base RGB, then applies the
    /// modifier chain in order.
    pub fn resolve_with_modifiers(
        &self,
        scheme_color: SchemeColor,
        modifiers: &[ColorModifier],
    ) -> Color {
        let base = self.resolve(scheme_color);
        apply_modifiers(base, modifiers)
    }
}

// ─── ThemeFontScheme ────────────────────────────────────────────────────────

/// A resolved font scheme holding the major (heading) and minor (body) font
/// families from the theme's `<a:fontScheme>` element.
///
/// In PresentationML, font references like `+mj-lt` (major Latin) and `+mn-lt`
/// (minor Latin) are resolved through this scheme.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ThemeFontScheme {
    /// Major (heading) Latin typeface, e.g. "Calibri Light".
    pub major_latin: String,
    /// Minor (body) Latin typeface, e.g. "Calibri".
    pub minor_latin: String,
}

impl ThemeFontScheme {
    /// The default Office theme font scheme (both major and minor are "Calibri"
    /// in the bundled theme).
    pub fn office_default() -> Self {
        Self {
            major_latin: "Calibri".to_string(),
            minor_latin: "Calibri".to_string(),
        }
    }

    /// Construct from explicit typeface names.
    pub fn new(major_latin: impl Into<String>, minor_latin: impl Into<String>) -> Self {
        Self {
            major_latin: major_latin.into(),
            minor_latin: minor_latin.into(),
        }
    }

    /// Resolve a font reference string to its actual typeface name.
    ///
    /// Recognized references:
    /// - `+mj-lt` → major Latin (heading)
    /// - `+mn-lt` → minor Latin (body)
    ///
    /// Returns `None` if the reference is not a recognized theme font ref.
    /// Non-theme font names (e.g. "Arial") should be used as-is by the caller.
    pub fn resolve_font_ref(&self, ref_str: &str) -> Option<&str> {
        match ref_str {
            "+mj-lt" => Some(&self.major_latin),
            "+mn-lt" => Some(&self.minor_latin),
            _ => None,
        }
    }
}

// ─── ColorMap ───────────────────────────────────────────────────────────────

/// A color map that remaps logical color names to scheme color slots.
///
/// In PresentationML, `<p:clrMap>` on the slide master defines the default
/// mapping, and `<p:clrMapOvr>` on a slide or layout can override individual
/// entries. The identity map (dk1→dk1, lt1→lt1, etc.) is the most common default.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ColorMap {
    pub dk1: SchemeColor,
    pub dk2: SchemeColor,
    pub lt1: SchemeColor,
    pub lt2: SchemeColor,
    pub accent1: SchemeColor,
    pub accent2: SchemeColor,
    pub accent3: SchemeColor,
    pub accent4: SchemeColor,
    pub accent5: SchemeColor,
    pub accent6: SchemeColor,
    pub hlink: SchemeColor,
    pub fol_hlink: SchemeColor,
}

impl ColorMap {
    /// The identity color map (each logical name maps to itself).
    pub fn identity() -> Self {
        Self {
            dk1: SchemeColor::Dk1,
            dk2: SchemeColor::Dk2,
            lt1: SchemeColor::Lt1,
            lt2: SchemeColor::Lt2,
            accent1: SchemeColor::Accent1,
            accent2: SchemeColor::Accent2,
            accent3: SchemeColor::Accent3,
            accent4: SchemeColor::Accent4,
            accent5: SchemeColor::Accent5,
            accent6: SchemeColor::Accent6,
            hlink: SchemeColor::Hlink,
            fol_hlink: SchemeColor::FolHlink,
        }
    }

    /// Remap a logical scheme color through this color map.
    ///
    /// For example, if the map has `dk1 → dk2`, then requesting `Dk1` returns
    /// `SchemeColor::Dk2`, which should then be resolved against the theme's
    /// color scheme.
    pub fn remap(&self, logical: SchemeColor) -> SchemeColor {
        match logical {
            SchemeColor::Dk1 => self.dk1,
            SchemeColor::Dk2 => self.dk2,
            SchemeColor::Lt1 => self.lt1,
            SchemeColor::Lt2 => self.lt2,
            SchemeColor::Accent1 => self.accent1,
            SchemeColor::Accent2 => self.accent2,
            SchemeColor::Accent3 => self.accent3,
            SchemeColor::Accent4 => self.accent4,
            SchemeColor::Accent5 => self.accent5,
            SchemeColor::Accent6 => self.accent6,
            SchemeColor::Hlink => self.hlink,
            SchemeColor::FolHlink => self.fol_hlink,
        }
    }

    /// Set a single mapping entry by logical name.
    ///
    /// `logical_name` is the attribute name (e.g. "dk1"), `target` is the
    /// scheme color it should map to. Returns `false` if the logical name is
    /// not recognized.
    pub fn set(&mut self, logical_name: &str, target: SchemeColor) -> bool {
        match logical_name {
            "dk1" => {
                self.dk1 = target;
                true
            }
            "dk2" => {
                self.dk2 = target;
                true
            }
            "lt1" => {
                self.lt1 = target;
                true
            }
            "lt2" => {
                self.lt2 = target;
                true
            }
            "accent1" => {
                self.accent1 = target;
                true
            }
            "accent2" => {
                self.accent2 = target;
                true
            }
            "accent3" => {
                self.accent3 = target;
                true
            }
            "accent4" => {
                self.accent4 = target;
                true
            }
            "accent5" => {
                self.accent5 = target;
                true
            }
            "accent6" => {
                self.accent6 = target;
                true
            }
            "hlink" => {
                self.hlink = target;
                true
            }
            "folHlink" => {
                self.fol_hlink = target;
                true
            }
            _ => false,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // ─── Existing ThemeColorScheme tests ────────────────────────────────────

    #[test]
    fn office_default_dk1_is_black() {
        let scheme = ThemeColorScheme::office_default();
        assert_eq!(scheme.resolve(SchemeColor::Dk1), Color::BLACK);
    }

    #[test]
    fn office_default_lt1_is_white() {
        let scheme = ThemeColorScheme::office_default();
        assert_eq!(scheme.resolve(SchemeColor::Lt1), Color::WHITE);
    }

    #[test]
    fn office_default_accents_match_theme_xml() {
        let scheme = ThemeColorScheme::office_default();
        assert_eq!(
            scheme.resolve(SchemeColor::Accent1),
            Color::from_hex("4F81BD").unwrap()
        );
        assert_eq!(
            scheme.resolve(SchemeColor::Accent2),
            Color::from_hex("C0504D").unwrap()
        );
        assert_eq!(
            scheme.resolve(SchemeColor::Accent3),
            Color::from_hex("9BBB59").unwrap()
        );
        assert_eq!(
            scheme.resolve(SchemeColor::Accent4),
            Color::from_hex("8064A2").unwrap()
        );
        assert_eq!(
            scheme.resolve(SchemeColor::Accent5),
            Color::from_hex("4BACC6").unwrap()
        );
        assert_eq!(
            scheme.resolve(SchemeColor::Accent6),
            Color::from_hex("F79646").unwrap()
        );
    }

    #[test]
    fn office_default_hlink_colors() {
        let scheme = ThemeColorScheme::office_default();
        assert_eq!(
            scheme.resolve(SchemeColor::Hlink),
            Color::from_hex("0000FF").unwrap()
        );
        assert_eq!(
            scheme.resolve(SchemeColor::FolHlink),
            Color::from_hex("800080").unwrap()
        );
    }

    #[test]
    fn resolve_all_variants_does_not_panic() {
        let scheme = ThemeColorScheme::office_default();
        for sc in SchemeColor::ALL {
            let _color = scheme.resolve(sc);
        }
    }

    #[test]
    fn from_hex_constructs_custom_scheme() {
        let scheme = ThemeColorScheme::from_hex(
            "111111", "222222", "333333", "444444", "555555", "666666", "777777", "888888",
            "999999", "AAAAAA", "BBBBBB", "CCCCCC",
        )
        .unwrap();
        assert_eq!(
            scheme.resolve(SchemeColor::Dk1),
            Color::from_hex("111111").unwrap()
        );
        assert_eq!(
            scheme.resolve(SchemeColor::FolHlink),
            Color::from_hex("CCCCCC").unwrap()
        );
    }

    #[test]
    fn from_hex_returns_none_on_invalid() {
        let result = ThemeColorScheme::from_hex(
            "ZZZZZZ", "222222", "333333", "444444", "555555", "666666", "777777", "888888",
            "999999", "AAAAAA", "BBBBBB", "CCCCCC",
        );
        assert!(result.is_none());
    }

    #[test]
    fn resolve_by_name_valid() {
        let scheme = ThemeColorScheme::office_default();
        assert_eq!(
            scheme.resolve_by_name("accent1"),
            Some(Color::from_hex("4F81BD").unwrap())
        );
        assert_eq!(scheme.resolve_by_name("dk1"), Some(Color::BLACK));
        assert_eq!(
            scheme.resolve_by_name("folHlink"),
            Some(Color::from_hex("800080").unwrap())
        );
    }

    #[test]
    fn resolve_by_name_invalid() {
        let scheme = ThemeColorScheme::office_default();
        assert_eq!(scheme.resolve_by_name("unknown"), None);
        assert_eq!(scheme.resolve_by_name(""), None);
    }

    #[test]
    fn custom_scheme_overrides_defaults() {
        let scheme = ThemeColorScheme::from_hex(
            "000000", "1F497D", "FFFFFF", "EEECE1", "FF0000", "C0504D", "9BBB59", "8064A2",
            "4BACC6", "F79646", "0000FF", "800080",
        )
        .unwrap();
        assert_eq!(
            scheme.resolve(SchemeColor::Accent1),
            Color { r: 255, g: 0, b: 0 }
        );
    }

    // ─── Tint/Shade/LumMod/LumOff tests ────────────────────────────────────

    #[test]
    fn tint_100000_is_identity() {
        // tint=100000 means no change
        let color = Color::from_hex("4F81BD").unwrap();
        let result = apply_tint(color, 100_000);
        assert_eq!(result, color);
    }

    #[test]
    fn tint_0_produces_white() {
        // tint=0 means fully toward white → luminance becomes 1.0
        let color = Color::from_hex("4F81BD").unwrap();
        let result = apply_tint(color, 0);
        assert_eq!(result, Color::WHITE);
    }

    #[test]
    fn tint_50000_lightens_color() {
        // tint=50000 should lighten the color (move halfway toward white)
        let color = Color::from_hex("0000FF").unwrap(); // pure blue
        let result = apply_tint(color, 50_000);
        // Luminance of pure blue is 0.5; new_l = 0.5*0.5 + 0.5 = 0.75
        // HSL(240, 1.0, 0.75) → a lighter blue
        assert!(result.r > 0); // should have some red from lightening
        assert!(result.g > 0); // should have some green from lightening
        assert_eq!(result.b, 255); // blue stays max
    }

    #[test]
    fn shade_100000_is_identity() {
        let color = Color::from_hex("4F81BD").unwrap();
        let result = apply_shade(color, 100_000);
        assert_eq!(result, color);
    }

    #[test]
    fn shade_0_produces_black() {
        let color = Color::from_hex("4F81BD").unwrap();
        let result = apply_shade(color, 0);
        assert_eq!(result, Color::BLACK);
    }

    #[test]
    fn shade_50000_darkens_color() {
        let color = Color::from_hex("0000FF").unwrap(); // pure blue, L=0.5
        let result = apply_shade(color, 50_000);
        // new_l = 0.5 * 0.5 = 0.25 → darker blue
        assert_eq!(result.r, 0);
        assert_eq!(result.g, 0);
        assert!(result.b < 255); // darker than original
        assert!(result.b > 0); // but not black
    }

    #[test]
    fn lum_mod_100000_is_identity() {
        let color = Color::from_hex("4F81BD").unwrap();
        let result = apply_lum_mod(color, 100_000);
        assert_eq!(result, color);
    }

    #[test]
    fn lum_mod_50000_halves_luminance() {
        let color = Color::from_hex("0000FF").unwrap(); // L=0.5
        let result = apply_lum_mod(color, 50_000);
        // new_l = 0.5 * 0.5 = 0.25
        assert_eq!(result.r, 0);
        assert_eq!(result.g, 0);
        assert!(result.b < 255);
        assert!(result.b > 0);
    }

    #[test]
    fn lum_mod_0_produces_black() {
        let color = Color::from_hex("FF8800").unwrap();
        let result = apply_lum_mod(color, 0);
        assert_eq!(result, Color::BLACK);
    }

    #[test]
    fn lum_off_positive_lightens() {
        let color = Color::BLACK; // L=0
        let result = apply_lum_off(color, 50_000); // add 0.5 to luminance
        // L becomes 0.5 → mid-gray
        assert_eq!(result.r, result.g);
        assert_eq!(result.g, result.b);
        assert!(result.r > 100 && result.r < 150); // approximately 128
    }

    #[test]
    fn lum_off_negative_darkens() {
        let color = Color::WHITE; // L=1.0
        let result = apply_lum_off(color, -50_000); // subtract 0.5
        // L becomes 0.5 → mid-gray
        assert_eq!(result.r, result.g);
        assert_eq!(result.g, result.b);
        assert!(result.r > 100 && result.r < 150);
    }

    #[test]
    fn lum_off_clamps_at_zero() {
        let color = Color::BLACK; // L=0
        let result = apply_lum_off(color, -50_000);
        assert_eq!(result, Color::BLACK);
    }

    #[test]
    fn lum_off_clamps_at_one() {
        let color = Color::WHITE; // L=1.0
        let result = apply_lum_off(color, 50_000);
        assert_eq!(result, Color::WHITE);
    }

    #[test]
    fn combined_modifiers_lum_mod_then_lum_off() {
        // Common pattern: lumMod=75000 + lumOff=25000 (lighten a dark color)
        let color = Color::from_hex("0000FF").unwrap(); // L=0.5
        let modifiers = &[ColorModifier::LumMod(75_000), ColorModifier::LumOff(25_000)];
        let result = apply_modifiers(color, modifiers);
        // After lumMod: L = 0.5 * 0.75 = 0.375
        // After lumOff: L = 0.375 + 0.25 = 0.625
        // Should be a lighter blue
        assert!(result.b > 128);
        assert!(result.r > 0 || result.g > 0); // lightened means some white mixed in
    }

    #[test]
    fn resolve_with_modifiers_applies_chain() {
        let scheme = ThemeColorScheme::office_default();
        // accent1 = 4F81BD, apply shade 50000
        let result =
            scheme.resolve_with_modifiers(SchemeColor::Accent1, &[ColorModifier::Shade(50_000)]);
        let base = scheme.resolve(SchemeColor::Accent1);
        // Result should be darker than base
        let base_lum = (base.r as u32 + base.g as u32 + base.b as u32) / 3;
        let result_lum = (result.r as u32 + result.g as u32 + result.b as u32) / 3;
        assert!(result_lum < base_lum);
    }

    #[test]
    fn resolve_with_empty_modifiers_is_identity() {
        let scheme = ThemeColorScheme::office_default();
        let base = scheme.resolve(SchemeColor::Accent3);
        let result = scheme.resolve_with_modifiers(SchemeColor::Accent3, &[]);
        assert_eq!(result, base);
    }

    // ─── ThemeFontScheme tests ──────────────────────────────────────────────

    #[test]
    fn font_scheme_office_default() {
        let fonts = ThemeFontScheme::office_default();
        assert_eq!(fonts.major_latin, "Calibri");
        assert_eq!(fonts.minor_latin, "Calibri");
    }

    #[test]
    fn font_scheme_custom() {
        let fonts = ThemeFontScheme::new("Calibri Light", "Calibri");
        assert_eq!(fonts.major_latin, "Calibri Light");
        assert_eq!(fonts.minor_latin, "Calibri");
    }

    #[test]
    fn resolve_font_ref_major() {
        let fonts = ThemeFontScheme::new("Calibri Light", "Calibri");
        assert_eq!(fonts.resolve_font_ref("+mj-lt"), Some("Calibri Light"));
    }

    #[test]
    fn resolve_font_ref_minor() {
        let fonts = ThemeFontScheme::new("Calibri Light", "Calibri");
        assert_eq!(fonts.resolve_font_ref("+mn-lt"), Some("Calibri"));
    }

    #[test]
    fn resolve_font_ref_unknown_returns_none() {
        let fonts = ThemeFontScheme::office_default();
        assert_eq!(fonts.resolve_font_ref("Arial"), None);
        assert_eq!(fonts.resolve_font_ref("+mj-ea"), None);
        assert_eq!(fonts.resolve_font_ref(""), None);
    }

    // ─── ColorMap tests ─────────────────────────────────────────────────────

    #[test]
    fn color_map_identity_is_passthrough() {
        let map = ColorMap::identity();
        for sc in SchemeColor::ALL {
            assert_eq!(map.remap(sc), sc);
        }
    }

    #[test]
    fn color_map_override_dk1_to_dk2() {
        let mut map = ColorMap::identity();
        assert!(map.set("dk1", SchemeColor::Dk2));
        assert_eq!(map.remap(SchemeColor::Dk1), SchemeColor::Dk2);
        // Other mappings unchanged
        assert_eq!(map.remap(SchemeColor::Lt1), SchemeColor::Lt1);
        assert_eq!(map.remap(SchemeColor::Accent1), SchemeColor::Accent1);
    }

    #[test]
    fn color_map_set_unknown_returns_false() {
        let mut map = ColorMap::identity();
        assert!(!map.set("unknown", SchemeColor::Dk1));
        assert!(!map.set("", SchemeColor::Dk1));
    }

    #[test]
    fn color_map_multiple_overrides() {
        let mut map = ColorMap::identity();
        map.set("dk1", SchemeColor::Dk2);
        map.set("lt1", SchemeColor::Lt2);
        map.set("accent1", SchemeColor::Accent6);
        assert_eq!(map.remap(SchemeColor::Dk1), SchemeColor::Dk2);
        assert_eq!(map.remap(SchemeColor::Lt1), SchemeColor::Lt2);
        assert_eq!(map.remap(SchemeColor::Accent1), SchemeColor::Accent6);
    }

    #[test]
    fn color_map_with_scheme_resolution() {
        // End-to-end: color map remaps dk1→dk2, then resolve through scheme
        let scheme = ThemeColorScheme::office_default();
        let mut map = ColorMap::identity();
        map.set("dk1", SchemeColor::Dk2);

        let remapped = map.remap(SchemeColor::Dk1);
        let color = scheme.resolve(remapped);
        // dk2 in office default is 1F497D
        assert_eq!(color, Color::from_hex("1F497D").unwrap());
    }

    // ─── HSL round-trip tests ───────────────────────────────────────────────

    #[test]
    fn hsl_round_trip_black() {
        let (h, s, l) = rgb_to_hsl(0, 0, 0);
        assert_eq!(l, 0.0);
        let (r, g, b) = hsl_to_rgb(h, s, l);
        assert_eq!((r, g, b), (0, 0, 0));
    }

    #[test]
    fn hsl_round_trip_white() {
        let (h, s, l) = rgb_to_hsl(255, 255, 255);
        assert_eq!(l, 1.0);
        let (r, g, b) = hsl_to_rgb(h, s, l);
        assert_eq!((r, g, b), (255, 255, 255));
    }

    #[test]
    fn hsl_round_trip_red() {
        let (h, s, l) = rgb_to_hsl(255, 0, 0);
        assert!((h - 0.0).abs() < 0.01);
        assert!((s - 1.0).abs() < 0.01);
        assert!((l - 0.5).abs() < 0.01);
        let (r, g, b) = hsl_to_rgb(h, s, l);
        assert_eq!((r, g, b), (255, 0, 0));
    }

    #[test]
    fn hsl_round_trip_arbitrary_color() {
        // 4F81BD → should round-trip
        let (h, s, l) = rgb_to_hsl(0x4F, 0x81, 0xBD);
        let (r, g, b) = hsl_to_rgb(h, s, l);
        assert_eq!((r, g, b), (0x4F, 0x81, 0xBD));
    }

    #[test]
    fn color_map_set_all_slots() {
        let mut map = ColorMap::identity();
        assert!(map.set("dk1", SchemeColor::Lt1));
        assert!(map.set("dk2", SchemeColor::Lt2));
        assert!(map.set("lt1", SchemeColor::Dk1));
        assert!(map.set("lt2", SchemeColor::Dk2));
        assert!(map.set("accent1", SchemeColor::Accent2));
        assert!(map.set("accent2", SchemeColor::Accent3));
        assert!(map.set("accent3", SchemeColor::Accent4));
        assert!(map.set("accent4", SchemeColor::Accent5));
        assert!(map.set("accent5", SchemeColor::Accent6));
        assert!(map.set("accent6", SchemeColor::Accent1));
        assert!(map.set("hlink", SchemeColor::FolHlink));
        assert!(map.set("folHlink", SchemeColor::Hlink));
        assert_eq!(map.remap(SchemeColor::Dk1), SchemeColor::Lt1);
        assert_eq!(map.remap(SchemeColor::Hlink), SchemeColor::FolHlink);
    }
}
