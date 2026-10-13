//! Theme application.
//!
//! A `ThemeSpec` customizes the deck's color scheme and fonts. To stay strictly
//! PowerPoint-valid, the theme part is the canonical Office theme with *targeted*
//! substitutions applied only for fields the spec sets — an empty spec yields the
//! byte-identical default.

use std::collections::HashMap;

use crate::template;

/// Canonical scheme colors in the bundled theme, keyed by scheme name.
/// (dk1/lt1 are sysClr and not substituted.)
const DEFAULTS: &[(&str, &str)] = &[
    ("dk2", "1F497D"),
    ("lt2", "EEECE1"),
    ("accent1", "4F81BD"),
    ("accent2", "C0504D"),
    ("accent3", "9BBB59"),
    ("accent4", "8064A2"),
    ("accent5", "4BACC6"),
    ("accent6", "F79646"),
    ("hlink", "0000FF"),
    ("folHlink", "800080"),
];

/// A theme customization. All fields optional; unset = Office default.
#[derive(Debug, Clone, Default)]
pub struct ThemeSpec {
    /// Convenience override for `accent1` (hex, with or without `#`).
    pub accent: Option<String>,
    pub heading_font: Option<String>,
    pub body_font: Option<String>,
    /// Per-scheme color overrides (dk2,lt2,accent1..6,hlink,folHlink) → hex.
    pub colors: HashMap<String, String>,
}

impl ThemeSpec {
    fn hex(s: &str) -> String {
        s.trim_start_matches('#').to_uppercase()
    }

    /// Effective hex for a scheme color name after applying overrides.
    pub fn resolve(&self, name: &str) -> Option<String> {
        if name == "accent1"
            && let Some(a) = &self.accent
        {
            return Some(Self::hex(a));
        }
        if let Some(c) = self.colors.get(name) {
            return Some(Self::hex(c));
        }
        DEFAULTS
            .iter()
            .find(|(k, _)| *k == name)
            .map(|(_, v)| v.to_string())
    }

    /// Build the theme XML by substituting into the canonical Office theme.
    pub fn build_theme_xml(&self) -> Vec<u8> {
        let mut xml = template::THEME_XML.to_string();

        for (name, default) in DEFAULTS {
            let resolved = self.resolve(name).unwrap_or_else(|| (*default).to_string());
            if resolved != *default {
                xml = xml.replace(
                    &format!("<a:{name}><a:srgbClr val=\"{default}\"/>"),
                    &format!("<a:{name}><a:srgbClr val=\"{resolved}\"/>"),
                );
            }
        }
        if let Some(f) = &self.heading_font {
            xml = xml.replace(
                "<a:majorFont><a:latin typeface=\"Calibri\"/>",
                &format!("<a:majorFont><a:latin typeface=\"{f}\"/>"),
            );
        }
        if let Some(f) = &self.body_font {
            xml = xml.replace(
                "<a:minorFont><a:latin typeface=\"Calibri\"/>",
                &format!("<a:minorFont><a:latin typeface=\"{f}\"/>"),
            );
        }
        xml.into_bytes()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn empty_spec_is_default_theme() {
        let xml = ThemeSpec::default().build_theme_xml();
        assert_eq!(xml, template::THEME_XML.as_bytes());
    }

    #[test]
    fn accent_and_fonts_substituted() {
        let mut t = ThemeSpec {
            accent: Some("#FF0000".into()),
            heading_font: Some("Inter".into()),
            body_font: Some("Inter".into()),
            ..Default::default()
        };
        t.colors.insert("accent3".into(), "00FF00".into());
        let xml = String::from_utf8(t.build_theme_xml()).unwrap();
        assert!(xml.contains("<a:accent1><a:srgbClr val=\"FF0000\"/>"));
        assert!(!xml.contains("4F81BD"));
        assert!(xml.contains("<a:accent3><a:srgbClr val=\"00FF00\"/>"));
        assert!(xml.contains("<a:majorFont><a:latin typeface=\"Inter\"/>"));
        assert!(xml.contains("<a:minorFont><a:latin typeface=\"Inter\"/>"));
    }

    #[test]
    fn resolve_defaults_and_overrides() {
        let t = ThemeSpec {
            accent: Some("aabbcc".into()),
            ..Default::default()
        };
        assert_eq!(t.resolve("accent1").as_deref(), Some("AABBCC"));
        assert_eq!(t.resolve("accent2").as_deref(), Some("C0504D"));
    }
}
