//! Parsing and writing of `[Content_Types].xml`.

use std::collections::HashMap;

use quick_xml::events::{BytesDecl, BytesEnd, BytesStart, Event};
use quick_xml::{Reader, Writer};

use crate::error::{OpcError, Result};

/// A single content type entry — either a Default (by extension) or an Override (by part name).
#[derive(Debug, Clone, PartialEq)]
pub enum ContentType {
    Default {
        extension: String,
        content_type: String,
    },
    Override {
        part_name: String,
        content_type: String,
    },
}

/// Parsed `[Content_Types].xml`.
#[derive(Debug, Clone)]
pub struct ContentTypes {
    pub defaults: HashMap<String, String>,
    pub overrides: HashMap<String, String>,
    /// Original bytes (when parsed from a file), emitted verbatim by `to_xml`
    /// until a mutation clears them — so an untouched package round-trips
    /// byte-for-byte.
    raw: Option<Vec<u8>>,
}

impl ContentTypes {
    /// Parse from XML bytes.
    pub fn from_xml(xml: &[u8]) -> Result<Self> {
        let mut reader = Reader::from_reader(xml);
        reader.config_mut().trim_text(true);

        let mut defaults = HashMap::new();
        let mut overrides = HashMap::new();
        let mut buf = Vec::new();

        loop {
            match reader.read_event_into(&mut buf) {
                Ok(Event::Empty(ref e)) => match e.name().as_ref() {
                    b"Default" => {
                        let mut ext = None;
                        let mut ct = None;
                        for attr in e.attributes() {
                            let attr = attr?;
                            match attr.key.as_ref() {
                                b"Extension" => {
                                    ext = Some(std::str::from_utf8(&attr.value)?.to_string());
                                }
                                b"ContentType" => {
                                    ct = Some(std::str::from_utf8(&attr.value)?.to_string());
                                }
                                _ => {}
                            }
                        }
                        match (ext, ct) {
                            (Some(e), Some(c)) => {
                                defaults.insert(e, c);
                            }
                            _ => return Err(OpcError::InvalidContentTypes),
                        }
                    }
                    b"Override" => {
                        let mut pn = None;
                        let mut ct = None;
                        for attr in e.attributes() {
                            let attr = attr?;
                            match attr.key.as_ref() {
                                b"PartName" => {
                                    pn = Some(std::str::from_utf8(&attr.value)?.to_string());
                                }
                                b"ContentType" => {
                                    ct = Some(std::str::from_utf8(&attr.value)?.to_string());
                                }
                                _ => {}
                            }
                        }
                        match (pn, ct) {
                            (Some(p), Some(c)) => {
                                overrides.insert(p, c);
                            }
                            _ => return Err(OpcError::InvalidContentTypes),
                        }
                    }
                    _ => {}
                },
                Ok(Event::Eof) => break,
                Err(e) => return Err(e.into()),
                _ => {}
            }
            buf.clear();
        }

        Ok(ContentTypes {
            defaults,
            overrides,
            raw: Some(xml.to_vec()),
        })
    }

    /// Serialize to XML bytes (verbatim when parsed and unmodified).
    pub fn to_xml(&self) -> Result<Vec<u8>> {
        if let Some(raw) = &self.raw {
            return Ok(raw.clone());
        }
        let mut writer = Writer::new_with_indent(Vec::new(), b' ', 2);

        writer.write_event(Event::Decl(BytesDecl::new(
            "1.0",
            Some("UTF-8"),
            Some("yes"),
        )))?;

        let mut types_start = BytesStart::new("Types");
        types_start.push_attribute((
            "xmlns",
            "http://schemas.openxmlformats.org/package/2006/content-types",
        ));
        writer.write_event(Event::Start(types_start))?;

        // Write defaults sorted for deterministic output
        let mut sorted_defaults: Vec<_> = self.defaults.iter().collect();
        sorted_defaults.sort_by_key(|(k, _)| (*k).clone());
        for (ext, ct) in sorted_defaults {
            let mut elem = BytesStart::new("Default");
            elem.push_attribute(("Extension", ext.as_str()));
            elem.push_attribute(("ContentType", ct.as_str()));
            writer.write_event(Event::Empty(elem))?;
        }

        // Write overrides sorted for deterministic output
        let mut sorted_overrides: Vec<_> = self.overrides.iter().collect();
        sorted_overrides.sort_by_key(|(k, _)| (*k).clone());
        for (pn, ct) in sorted_overrides {
            let mut elem = BytesStart::new("Override");
            elem.push_attribute(("PartName", pn.as_str()));
            elem.push_attribute(("ContentType", ct.as_str()));
            writer.write_event(Event::Empty(elem))?;
        }

        writer.write_event(Event::End(BytesEnd::new("Types")))?;

        Ok(writer.into_inner())
    }

    /// Look up the content type for a given part name.
    pub fn content_type_for(&self, part_name: &str) -> Option<&str> {
        // Check overrides first
        if let Some(ct) = self.overrides.get(part_name) {
            return Some(ct.as_str());
        }
        // Fall back to defaults by extension
        if let Some(dot_pos) = part_name.rfind('.') {
            let ext = &part_name[dot_pos + 1..];
            if let Some(ct) = self.defaults.get(ext) {
                return Some(ct.as_str());
            }
        }
        None
    }

    /// Add a default content type for an extension (e.g., "png" -> "image/png").
    pub fn add_default(&mut self, extension: &str, content_type: &str) {
        if !self.defaults.contains_key(extension) {
            self.defaults
                .insert(extension.to_string(), content_type.to_string());
            self.raw = None; // structure changed → re-serialize
        }
    }

    /// Add an override content type for a specific part name.
    pub fn add_override(&mut self, part_name: &str, content_type: &str) {
        let changed = self
            .overrides
            .insert(part_name.to_string(), content_type.to_string())
            != Some(content_type.to_string());
        if changed {
            self.raw = None;
        }
    }

    /// Remove a part's override content type (e.g. when pruning a deleted part).
    pub fn remove_override(&mut self, part_name: &str) {
        if self.overrides.remove(part_name).is_some() {
            self.raw = None;
        }
    }

    /// Create a new ContentTypes with the standard PPTX defaults
    /// (rels + xml extensions, and the main presentation part override).
    pub fn new_pptx() -> Self {
        let mut defaults = HashMap::new();
        defaults.insert(
            "rels".to_string(),
            "application/vnd.openxmlformats-package.relationships+xml".to_string(),
        );
        defaults.insert("xml".to_string(), "application/xml".to_string());

        let mut overrides = HashMap::new();
        overrides.insert(
            "/ppt/presentation.xml".to_string(),
            "application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"
                .to_string(),
        );

        ContentTypes {
            defaults,
            overrides,
            raw: None,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn round_trip_content_types() {
        let ct = ContentTypes::new_pptx();
        let xml = ct.to_xml().unwrap();
        let parsed = ContentTypes::from_xml(&xml).unwrap();
        assert_eq!(parsed.defaults.len(), ct.defaults.len());
        assert_eq!(parsed.overrides.len(), ct.overrides.len());
        assert_eq!(
            parsed.content_type_for("/ppt/presentation.xml"),
            Some(
                "application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"
            )
        );
    }

    #[test]
    fn lookup_by_extension() {
        let ct = ContentTypes::new_pptx();
        assert_eq!(
            ct.content_type_for("/ppt/_rels/presentation.xml.rels"),
            Some("application/vnd.openxmlformats-package.relationships+xml")
        );
    }
}
