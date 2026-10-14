//! Defined names follow their sheet (ADR-0032 P4). The xlsx library renames and
//! removes a sheet without touching the workbook's defined names, so a name
//! scoped to the sheet (`localSheetId`: an autofilter's `_xlnm._FilterDatabase`,
//! a print area) would end up on the wrong sheet or refer to a name that is
//! gone. [`crate::Workbook`] remembers each rename and delete and applies them
//! to `xl/workbook.xml` of the package it saves, as a raw-part edit between the
//! library's save and the validation gate:
//!
//! * delete: the names scoped to the deleted sheet are dropped, and a scope
//!   above it moves down by one;
//! * rename: inside the names scoped to that sheet, references written with
//!   the old name are rewritten with the new one, quoted when it needs it.
//!
//! A name scoped elsewhere that refers to the sheet is not rewritten: the tools
//! refuse the edit before it starts.

use std::fmt::Write as _;
use std::io::{Cursor, Read, Write};

use zip::{CompressionMethod, ZipArchive, ZipWriter, write::SimpleFileOptions};

use crate::error::{Limit, OfficeError, Result, limit};
use crate::limits::Limits;
use crate::sheetrefs::rename_in_formula;

/// One change to the sheets that defined names must follow.
#[derive(Debug, Clone)]
pub(crate) enum NameOp {
    /// The sheet at this position was removed.
    Delete(usize),
    /// The sheet at this position was renamed.
    Rename {
        index: usize,
        old: String,
        new: String,
    },
}

/// Whether `name` ends in `.ext`, ignoring case.
pub(crate) fn ext_is(name: &str, ext: &str) -> bool {
    std::path::Path::new(name)
        .extension()
        .is_some_and(|e| e.eq_ignore_ascii_case(ext))
}

const PART: &str = "xl/workbook.xml";

/// `package` with the defined names of `xl/workbook.xml` following `ops`, in
/// order.
pub(crate) fn apply(package: &[u8], ops: &[NameOp], l: &Limits) -> Result<Vec<u8>> {
    let mut input = ZipArchive::new(Cursor::new(package))
        .map_err(|e| OfficeError::Malformed(format!("package: {e}")))?;
    let mut out = ZipWriter::new(Cursor::new(Vec::new()));
    for i in 0..input.len() {
        let entry = input
            .by_index(i)
            .map_err(|e| OfficeError::Malformed(format!("package: {e}")))?;
        if entry.is_dir() {
            continue;
        }
        let name = entry.name().to_owned();
        // A part over the cap is an error, never a shortened part.
        let cap = if ext_is(&name, "xml") || ext_is(&name, "rels") {
            l.max_xml_large_part_bytes_xlsx
        } else {
            l.max_media_part_bytes
        };
        let mut data = Vec::new();
        entry
            .take(cap.saturating_add(1))
            .read_to_end(&mut data)
            .map_err(|e| OfficeError::Malformed(format!("part {name}: {e}")))?;
        if data.len() as u64 > cap {
            return Err(limit(Limit::XmlPartBytes, data.len() as u64));
        }
        if name == PART
            && let Ok(text) = std::str::from_utf8(&data)
        {
            data = rewrite(text, ops).into_bytes();
        }
        out.start_file(
            name.as_str(),
            SimpleFileOptions::default().compression_method(CompressionMethod::Deflated),
        )
        .map_err(|e| OfficeError::Internal(format!("repack: {e}")))?;
        out.write_all(&data)
            .map_err(|e| OfficeError::Internal(format!("repack: {e}")))?;
    }
    out.finish()
        .map(Cursor::into_inner)
        .map_err(|e| OfficeError::Internal(format!("repack: {e}")))
}

struct Item {
    /// The attributes as written, between the tag name and `>`.
    attrs: String,
    local: Option<usize>,
    /// The formula, entities decoded.
    text: String,
}

fn local_of(attrs: &str) -> Option<usize> {
    let at = attrs.find("localSheetId=\"")? + "localSheetId=\"".len();
    attrs[at..].split('"').next()?.trim().parse().ok()
}

fn with_local(attrs: &str, to: usize) -> String {
    let Some(at) = attrs.find("localSheetId=\"") else {
        return attrs.to_owned();
    };
    let from = at + "localSheetId=\"".len();
    let end = from + attrs[from..].find('"').unwrap_or(0);
    format!("{}{}{}", &attrs[..from], to, &attrs[end..])
}

fn unescape(s: &str) -> String {
    s.replace("&lt;", "<")
        .replace("&gt;", ">")
        .replace("&quot;", "\"")
        .replace("&apos;", "'")
        .replace("&amp;", "&")
}

fn escape(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
}

fn rewrite(xml: &str, ops: &[NameOp]) -> String {
    let Some(open) = xml.find("<definedNames>") else {
        return xml.to_owned();
    };
    let body_from = open + "<definedNames>".len();
    let Some(close_rel) = xml[body_from..].find("</definedNames>") else {
        return xml.to_owned();
    };
    let body = &xml[body_from..body_from + close_rel];
    let mut items: Vec<Item> = Vec::new();
    let mut rest = body;
    while let Some(at) = rest.find("<definedName") {
        let after = &rest[at + "<definedName".len()..];
        let Some(gt) = after.find('>') else { break };
        let attrs = after[..gt].trim_end_matches('/').trim().to_owned();
        if after[..gt].ends_with('/') {
            items.push(Item {
                local: local_of(&attrs),
                attrs,
                text: String::new(),
            });
            rest = &after[gt + 1..];
            continue;
        }
        let Some(end) = after[gt + 1..].find("</definedName>") else {
            break;
        };
        items.push(Item {
            local: local_of(&attrs),
            attrs,
            text: unescape(&after[gt + 1..gt + 1 + end]),
        });
        rest = &after[gt + 1 + end + "</definedName>".len()..];
    }
    for op in ops {
        match op {
            NameOp::Delete(index) => {
                items.retain(|i| i.local != Some(*index));
                for item in &mut items {
                    if let Some(l) = item.local
                        && l > *index
                    {
                        item.attrs = with_local(&item.attrs, l - 1);
                        item.local = Some(l - 1);
                    }
                }
            }
            NameOp::Rename { index, old, new } => {
                for item in items.iter_mut().filter(|i| i.local == Some(*index)) {
                    item.text = rename_in_formula(&item.text, old, new);
                }
            }
        }
    }
    let mut region = String::new();
    for i in &items {
        let _ = write!(
            region,
            "<definedName {}>{}</definedName>",
            i.attrs,
            escape(&i.text)
        );
    }
    if items.is_empty() {
        // `<definedNames/>` would be an empty list, which the format forbids.
        return format!(
            "{}{}",
            &xml[..open],
            &xml[body_from + close_rel + "</definedNames>".len()..]
        );
    }
    format!(
        "{}<definedNames>{region}</definedNames>{}",
        &xml[..open],
        &xml[body_from + close_rel + "</definedNames>".len()..]
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    const XML: &str = "<workbook><sheets/><definedNames>\
        <definedName name=\"_xlnm._FilterDatabase\" localSheetId=\"1\">Old!$A$1:$C$5</definedName>\
        <definedName name=\"_xlnm.Print_Area\" localSheetId=\"2\">'Third'!$A$1</definedName>\
        <definedName name=\"G\">Old!$A$1</definedName></definedNames><calcPr/></workbook>";

    #[test]
    fn a_delete_drops_the_scoped_names_and_moves_later_scopes_down() {
        let out = rewrite(XML, &[NameOp::Delete(1)]);
        assert!(!out.contains("_FilterDatabase"), "{out}");
        assert!(out.contains("localSheetId=\"1\">'Third'!$A$1"), "{out}");
        assert!(
            out.contains("<definedName name=\"G\">Old!$A$1</definedName>"),
            "{out}"
        );
        // An earlier sheet's delete moves both.
        let out = rewrite(XML, &[NameOp::Delete(0)]);
        assert!(out.contains("localSheetId=\"0\">Old!$A$1:$C$5"), "{out}");
        assert!(out.contains("localSheetId=\"1\">'Third'"), "{out}");
    }

    #[test]
    fn a_rename_rewrites_only_the_names_scoped_to_that_sheet() {
        let out = rewrite(
            XML,
            &[NameOp::Rename {
                index: 1,
                old: "Old".into(),
                new: "New Name".into(),
            }],
        );
        assert!(
            out.contains("localSheetId=\"1\">'New Name'!$A$1:$C$5"),
            "{out}"
        );
        // The workbook-scoped name is the tools' to refuse, not ours to touch.
        assert!(out.contains("name=\"G\">Old!$A$1<"), "{out}");
    }

    #[test]
    fn the_last_name_removed_removes_the_list() {
        let one = "<workbook><definedNames><definedName name=\"a\" localSheetId=\"0\">S!A1</definedName></definedNames><calcPr/></workbook>";
        assert_eq!(
            rewrite(one, &[NameOp::Delete(0)]),
            "<workbook><calcPr/></workbook>"
        );
    }

    #[test]
    fn entities_survive() {
        let xml = "<workbook><definedNames><definedName name=\"a\" localSheetId=\"0\">IF(Old!A1&gt;1,&quot;x&quot;,0)</definedName></definedNames></workbook>";
        let out = rewrite(
            xml,
            &[NameOp::Rename {
                index: 0,
                old: "Old".into(),
                new: "N".into(),
            }],
        );
        assert!(out.contains("IF(N!A1&gt;1,\"x\",0)"), "{out}");
    }
}
