//! Core document properties (`docProps/core.xml`) — read/write with byte-preserving
//! fallback for unset fields on opened decks.

use zavora_slide_oxml::{Document, Node};

/// Core document properties as defined in OPC / Dublin Core.
///
/// All fields are `Option<String>`. When writing, only `Some` fields are updated;
/// `None` fields preserve existing values in the XML (byte-preserving for opened decks).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct CoreProperties {
    pub title: Option<String>,
    pub author: Option<String>,
    pub subject: Option<String>,
    pub keywords: Option<String>,
    pub comments: Option<String>,
    pub category: Option<String>,
    pub created: Option<String>,
    pub modified: Option<String>,
    pub last_modified_by: Option<String>,
}

/// Parse `CoreProperties` from `docProps/core.xml` bytes.
pub(crate) fn parse_core_properties(xml: &[u8]) -> CoreProperties {
    let doc = match Document::parse(xml) {
        Ok(d) => d,
        Err(_) => return CoreProperties::default(),
    };
    let root = match doc.root() {
        Some(r) => r,
        None => return CoreProperties::default(),
    };

    let mut props = CoreProperties::default();

    for child in &root.children {
        let Node::Element(el) = child else { continue };
        let local = el.local_name();
        let text = el.text_content();
        let text = if text.is_empty() { None } else { Some(text) };

        match local {
            b"title" => props.title = text,
            b"creator" => props.author = text,
            b"subject" => props.subject = text,
            b"keywords" => props.keywords = text,
            b"description" => props.comments = text,
            b"category" => props.category = text,
            b"created" => props.created = text,
            b"modified" => props.modified = text,
            b"lastModifiedBy" => props.last_modified_by = text,
            _ => {}
        }
    }

    props
}

/// Apply `CoreProperties` to existing `docProps/core.xml` bytes, returning the
/// updated XML. Only fields that are `Some` are written; `None` fields preserve
/// existing values byte-for-byte.
pub(crate) fn apply_core_properties(xml: &[u8], props: &CoreProperties) -> Vec<u8> {
    let mut doc = match Document::parse(xml) {
        Ok(d) => d,
        Err(_) => return xml.to_vec(),
    };
    let root = match doc.root_mut() {
        Some(r) => r,
        None => return xml.to_vec(),
    };

    // For each property that is Some, find or create the element and set its text.
    let fields: &[(&Option<String>, &[u8], &[u8])] = &[
        (&props.title, b"title", b"dc:title"),
        (&props.author, b"creator", b"dc:creator"),
        (&props.subject, b"subject", b"dc:subject"),
        (&props.keywords, b"keywords", b"cp:keywords"),
        (&props.comments, b"description", b"dc:description"),
        (&props.category, b"category", b"cp:category"),
        (&props.created, b"created", b"dcterms:created"),
        (&props.modified, b"modified", b"dcterms:modified"),
        (
            &props.last_modified_by,
            b"lastModifiedBy",
            b"cp:lastModifiedBy",
        ),
    ];

    for (value, local_name, qualified_name) in fields {
        let Some(val) = value else { continue };

        // Try to find existing element by local name.
        let found = root.children.iter_mut().any(|n| {
            if let Node::Element(el) = n
                && el.local_name() == *local_name
            {
                el.set_text_content(val);
                return true;
            }
            false
        });

        if !found {
            // Create a new element. For dcterms:created/modified, include xsi:type attr.
            let xml_str = if *local_name == b"created" || *local_name == b"modified" {
                format!(
                    "<{} xsi:type=\"dcterms:W3CDTF\">{}</{}>",
                    std::str::from_utf8(qualified_name).unwrap_or(""),
                    xml_escape(val),
                    std::str::from_utf8(qualified_name).unwrap_or("")
                )
            } else {
                format!(
                    "<{}>{}</{}>",
                    std::str::from_utf8(qualified_name).unwrap_or(""),
                    xml_escape(val),
                    std::str::from_utf8(qualified_name).unwrap_or("")
                )
            };
            // Parse the fragment and append it.
            if let Ok(frag) = Document::parse(xml_str.as_bytes()) {
                for node in frag.nodes {
                    if matches!(node, Node::Element(_)) {
                        root.children.push(node);
                        break;
                    }
                }
            }
        }
    }

    doc.to_bytes()
}

/// Build a default `docProps/core.xml` with the given properties applied.
pub(crate) fn build_core_xml(props: &CoreProperties) -> Vec<u8> {
    let default_xml = crate::template::CORE_XML.as_bytes();
    apply_core_properties(default_xml, props)
}

/// Minimal XML escaping for text content.
fn xml_escape(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
}

#[cfg(test)]
mod tests {
    use super::*;

    const SAMPLE_CORE: &[u8] = br#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:dcmitype="http://purl.org/dc/dcmitype/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><dc:title>My Deck</dc:title><dc:creator>Alice</dc:creator><dc:subject>Testing</dc:subject><cp:keywords>rust, pptx</cp:keywords><dc:description>A test deck</dc:description><cp:category>Demo</cp:category><dcterms:created xsi:type="dcterms:W3CDTF">2024-01-01T00:00:00Z</dcterms:created><dcterms:modified xsi:type="dcterms:W3CDTF">2024-06-15T12:00:00Z</dcterms:modified><cp:lastModifiedBy>Bob</cp:lastModifiedBy></cp:coreProperties>"#;

    #[test]
    fn parse_reads_all_fields() {
        let props = parse_core_properties(SAMPLE_CORE);
        assert_eq!(props.title.as_deref(), Some("My Deck"));
        assert_eq!(props.author.as_deref(), Some("Alice"));
        assert_eq!(props.subject.as_deref(), Some("Testing"));
        assert_eq!(props.keywords.as_deref(), Some("rust, pptx"));
        assert_eq!(props.comments.as_deref(), Some("A test deck"));
        assert_eq!(props.category.as_deref(), Some("Demo"));
        assert_eq!(props.created.as_deref(), Some("2024-01-01T00:00:00Z"));
        assert_eq!(props.modified.as_deref(), Some("2024-06-15T12:00:00Z"));
        assert_eq!(props.last_modified_by.as_deref(), Some("Bob"));
    }

    #[test]
    fn parse_empty_fields_are_none() {
        let xml = br#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:dcmitype="http://purl.org/dc/dcmitype/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><dc:title></dc:title></cp:coreProperties>"#;
        let props = parse_core_properties(xml);
        assert_eq!(props.title, None); // empty text → None
        assert_eq!(props.author, None);
    }

    #[test]
    fn apply_only_modifies_set_fields() {
        let update = CoreProperties {
            title: Some("New Title".into()),
            ..Default::default()
        };
        let result = apply_core_properties(SAMPLE_CORE, &update);
        let re_parsed = parse_core_properties(&result);
        assert_eq!(re_parsed.title.as_deref(), Some("New Title"));
        // Unset fields preserved.
        assert_eq!(re_parsed.author.as_deref(), Some("Alice"));
        assert_eq!(re_parsed.subject.as_deref(), Some("Testing"));
        assert_eq!(re_parsed.keywords.as_deref(), Some("rust, pptx"));
        assert_eq!(re_parsed.comments.as_deref(), Some("A test deck"));
        assert_eq!(re_parsed.category.as_deref(), Some("Demo"));
        assert_eq!(re_parsed.last_modified_by.as_deref(), Some("Bob"));
    }

    #[test]
    fn apply_creates_missing_elements() {
        let minimal = br#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:dcmitype="http://purl.org/dc/dcmitype/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"></cp:coreProperties>"#;
        let update = CoreProperties {
            title: Some("Hello".into()),
            category: Some("Test".into()),
            created: Some("2024-01-01T00:00:00Z".into()),
            ..Default::default()
        };
        let result = apply_core_properties(minimal, &update);
        let re_parsed = parse_core_properties(&result);
        assert_eq!(re_parsed.title.as_deref(), Some("Hello"));
        assert_eq!(re_parsed.category.as_deref(), Some("Test"));
        assert_eq!(re_parsed.created.as_deref(), Some("2024-01-01T00:00:00Z"));
    }

    #[test]
    fn default_core_xml_has_engine_defaults() {
        let props = parse_core_properties(crate::template::CORE_XML.as_bytes());
        assert_eq!(props.author.as_deref(), Some("zavora-slide"));
        assert_eq!(props.last_modified_by.as_deref(), Some("zavora-slide"));
    }
}
