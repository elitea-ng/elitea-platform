//! OPC Package — read/write ZIP-based OOXML packages.

use std::collections::HashMap;
use std::io::{Read, Seek, Write};
use std::path::Path;

use zip::ZipWriter;
use zip::read::ZipArchive;
use zip::write::SimpleFileOptions;

use crate::content_types::ContentTypes;
use crate::error::{OpcError, Result};
use crate::relationship::{Relationships, rel_types};

/// A single part within the OPC package.
#[derive(Debug, Clone)]
pub struct PackagePart {
    /// Part name (URI), e.g. "/ppt/presentation.xml"
    pub name: String,
    /// Raw bytes of the part content
    pub data: Vec<u8>,
}

/// An in-memory representation of an OPC package (ZIP archive).
#[derive(Debug, Clone)]
pub struct OpcPackage {
    /// Content types from `[Content_Types].xml`
    pub content_types: ContentTypes,
    /// Package-level relationships from `_rels/.rels`
    pub package_rels: Relationships,
    /// Part-level relationships keyed by the part they belong to
    /// (e.g. "/ppt/presentation.xml").
    pub part_rels: HashMap<String, Relationships>,
    /// All parts keyed by their URI (e.g. "/ppt/presentation.xml").
    pub parts: HashMap<String, Vec<u8>>,
}

impl OpcPackage {
    /// Open an OPC package from a file path.
    pub fn open<P: AsRef<Path>>(path: P) -> Result<Self> {
        let file = std::fs::File::open(path)?;
        Self::from_reader(file)
    }

    /// Open an OPC package from any reader that implements Read + Seek.
    pub fn from_reader<R: Read + Seek>(reader: R) -> Result<Self> {
        let mut archive = ZipArchive::new(reader)?;
        let mut raw_parts: HashMap<String, Vec<u8>> = HashMap::new();

        for i in 0..archive.len() {
            let mut entry = archive.by_index(i)?;
            if entry.is_dir() {
                continue;
            }
            // Elitea patch (zip 8 -> 9): `ZipFile::name` returns
            // `ZipResult<Cow<'_, str>>` in zip 9. zip 8 returned `&str` and
            // hid a decode failure. `?` maps the error through
            // `OpcError::Zip`. The string value does not change.
            let name = entry.name()?.to_string();
            let mut data = Vec::new();
            entry.read_to_end(&mut data)?;
            raw_parts.insert(name, data);
        }

        let ct_xml = raw_parts
            .get("[Content_Types].xml")
            .ok_or_else(|| OpcError::PartNotFound("[Content_Types].xml".into()))?;
        let content_types = ContentTypes::from_xml(ct_xml)?;

        let package_rels = if let Some(rels_xml) = raw_parts.get("_rels/.rels") {
            Relationships::from_xml(rels_xml)?
        } else {
            Relationships::new()
        };

        let mut part_rels = HashMap::new();
        let rels_entries: Vec<String> = raw_parts
            .keys()
            .filter(|k| k.ends_with(".rels") && *k != "_rels/.rels")
            .cloned()
            .collect();

        for rels_path in rels_entries {
            if let Some(xml_data) = raw_parts.get(&rels_path) {
                let rels = Relationships::from_xml(xml_data)?;
                let part_name = rels_path_to_part_name(&rels_path);
                part_rels.insert(part_name, rels);
            }
        }

        let mut parts = HashMap::new();
        for (name, data) in &raw_parts {
            if name == "[Content_Types].xml" || name == "_rels/.rels" || name.ends_with(".rels") {
                continue;
            }
            let normalized = if name.starts_with('/') {
                name.clone()
            } else {
                format!("/{name}")
            };
            parts.insert(normalized, data.clone());
        }

        Ok(OpcPackage {
            content_types,
            package_rels,
            part_rels,
            parts,
        })
    }

    /// Save the OPC package to a file path.
    pub fn save<P: AsRef<Path>>(&self, path: P) -> Result<()> {
        let file = std::fs::File::create(path)?;
        self.write_to(file)
    }

    /// Write the OPC package to any writer.
    pub fn write_to<W: Write + Seek>(&self, writer: W) -> Result<()> {
        let mut zip = ZipWriter::new(writer);
        let options =
            SimpleFileOptions::default().compression_method(zip::CompressionMethod::Deflated);

        let ct_xml = self.content_types.to_xml()?;
        zip.start_file("[Content_Types].xml", options)?;
        zip.write_all(&ct_xml)?;

        let pkg_rels_xml = self.package_rels.to_xml()?;
        zip.start_file("_rels/.rels", options)?;
        zip.write_all(&pkg_rels_xml)?;

        for (part_name, rels) in &self.part_rels {
            let rels_path = part_name_to_rels_path(part_name);
            let rels_xml = rels.to_xml()?;
            zip.start_file(&rels_path, options)?;
            zip.write_all(&rels_xml)?;
        }

        for (name, data) in &self.parts {
            let zip_name = name.strip_prefix('/').unwrap_or(name);
            zip.start_file(zip_name, options)?;
            zip.write_all(data)?;
        }

        zip.finish()?;
        Ok(())
    }

    /// Get raw bytes of a part by its URI.
    pub fn get_part(&self, part_name: &str) -> Option<&[u8]> {
        self.parts.get(part_name).map(|v| v.as_slice())
    }

    /// Set (or replace) a part's raw bytes.
    pub fn set_part(&mut self, part_name: &str, data: Vec<u8>) {
        self.parts.insert(part_name.to_string(), data);
    }

    /// Remove a part by its URI; returns true if it existed.
    pub fn remove_part(&mut self, part_name: &str) -> bool {
        self.parts.remove(part_name).is_some()
    }

    /// Iterate over all part URIs in the package.
    pub fn part_names(&self) -> impl Iterator<Item = &str> {
        self.parts.keys().map(|s| s.as_str())
    }

    /// Get the relationships for a specific part.
    pub fn get_part_rels(&self, part_name: &str) -> Option<&Relationships> {
        self.part_rels.get(part_name)
    }

    /// Get or create the relationships for a specific part.
    pub fn get_or_create_part_rels(&mut self, part_name: &str) -> &mut Relationships {
        self.part_rels.entry(part_name.to_string()).or_default()
    }

    /// Resolve the target URI of a relationship relative to its source part.
    pub fn resolve_rel_target(source_part: &str, rel_target: &str) -> String {
        if rel_target.starts_with('/') {
            return rel_target.to_string();
        }
        let dir = if let Some(pos) = source_part.rfind('/') {
            &source_part[..=pos]
        } else {
            "/"
        };
        format!("{dir}{rel_target}")
    }

    /// Find the main presentation part URI from package relationships.
    pub fn main_presentation_part(&self) -> Option<String> {
        self.package_rels
            .get_by_type(rel_types::PRESENTATION)
            .map(|rel| {
                if rel.target.starts_with('/') {
                    rel.target.clone()
                } else {
                    format!("/{}", rel.target)
                }
            })
    }

    /// Create a new, minimal OPC package suitable for a .pptx file.
    pub fn new_pptx() -> Self {
        let content_types = ContentTypes::new_pptx();
        let mut package_rels = Relationships::new();
        package_rels.add(rel_types::PRESENTATION, "ppt/presentation.xml");

        OpcPackage {
            content_types,
            package_rels,
            part_rels: HashMap::new(),
            parts: HashMap::new(),
        }
    }
}

/// Convert a .rels file path to the part name it belongs to.
/// e.g. "ppt/_rels/presentation.xml.rels" → "/ppt/presentation.xml"
fn rels_path_to_part_name(rels_path: &str) -> String {
    let path = rels_path
        .replace("_rels/", "")
        .trim_end_matches(".rels")
        .to_string();
    if path.starts_with('/') {
        path
    } else {
        format!("/{path}")
    }
}

/// Convert a part name to its .rels file path.
/// e.g. "/ppt/presentation.xml" → "ppt/_rels/presentation.xml.rels"
fn part_name_to_rels_path(part_name: &str) -> String {
    let name = part_name.strip_prefix('/').unwrap_or(part_name);
    if let Some(pos) = name.rfind('/') {
        let dir = &name[..pos];
        let file = &name[pos + 1..];
        format!("{dir}/_rels/{file}.rels")
    } else {
        format!("_rels/{name}.rels")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rels_path_conversion() {
        assert_eq!(
            rels_path_to_part_name("ppt/_rels/presentation.xml.rels"),
            "/ppt/presentation.xml"
        );
        assert_eq!(
            part_name_to_rels_path("/ppt/presentation.xml"),
            "ppt/_rels/presentation.xml.rels"
        );
    }

    #[test]
    fn resolve_relative_target() {
        assert_eq!(
            OpcPackage::resolve_rel_target("/ppt/presentation.xml", "slides/slide1.xml"),
            "/ppt/slides/slide1.xml"
        );
        assert_eq!(
            OpcPackage::resolve_rel_target("/ppt/presentation.xml", "/ppt/theme/theme1.xml"),
            "/ppt/theme/theme1.xml"
        );
    }

    #[test]
    fn new_pptx_package() {
        let pkg = OpcPackage::new_pptx();
        assert_eq!(
            pkg.main_presentation_part().unwrap(),
            "/ppt/presentation.xml"
        );
    }

    #[test]
    fn round_trip_package() {
        let mut pkg = OpcPackage::new_pptx();
        pkg.set_part("/ppt/presentation.xml", b"<presentation/>".to_vec());
        pkg.get_or_create_part_rels("/ppt/presentation.xml")
            .add(rel_types::SLIDE, "slides/slide1.xml");

        // Write to memory
        let mut buf = std::io::Cursor::new(Vec::new());
        pkg.write_to(&mut buf).unwrap();

        // Read back
        buf.set_position(0);
        let pkg2 = OpcPackage::from_reader(buf).unwrap();
        assert_eq!(
            pkg2.get_part("/ppt/presentation.xml"),
            Some(b"<presentation/>".as_slice())
        );
        assert_eq!(
            pkg2.main_presentation_part().unwrap(),
            "/ppt/presentation.xml"
        );
        // Part-level rels survived the round trip.
        let rels = pkg2.get_part_rels("/ppt/presentation.xml").unwrap();
        assert_eq!(
            rels.get_by_type(rel_types::SLIDE).unwrap().target,
            "slides/slide1.xml"
        );
        // Iteration sees the presentation part.
        assert!(pkg2.part_names().any(|n| n == "/ppt/presentation.xml"));
    }
}
