//! Fixtures with embedded pictures, generated in the test (no committed
//! binaries): a PNG built from a solid colour, a DOCX, a PPTX and a PDF that
//! carry such pictures between sentences the position tests look for.

#![allow(
    dead_code,
    clippy::format_collect,
    reason = "each test binary uses a part of the fixtures; the XML is assembled from small formatted pieces"
)]

use std::io::Write as _;

fn crc32(data: &[u8]) -> u32 {
    let mut crc = !0_u32;
    for &byte in data {
        crc ^= u32::from(byte);
        for _ in 0..8 {
            crc = if crc & 1 != 0 {
                (crc >> 1) ^ 0xEDB8_8320
            } else {
                crc >> 1
            };
        }
    }
    !crc
}

fn png_chunk(kind: [u8; 4], body: &[u8]) -> Vec<u8> {
    let mut out = u32::try_from(body.len())
        .expect("small chunk")
        .to_be_bytes()
        .to_vec();
    let mut tail = kind.to_vec();
    tail.extend_from_slice(body);
    out.extend_from_slice(&tail);
    out.extend_from_slice(&crc32(&tail).to_be_bytes());
    out
}

/// A valid 1x1 PNG of one colour (a stored-deflate IDAT: no compressor).
pub fn png(rgb: [u8; 3]) -> Vec<u8> {
    let mut out = b"\x89PNG\r\n\x1a\n".to_vec();
    let mut header = Vec::new();
    header.extend_from_slice(&1_u32.to_be_bytes());
    header.extend_from_slice(&1_u32.to_be_bytes());
    header.extend_from_slice(&[8, 2, 0, 0, 0]);
    out.extend(png_chunk(*b"IHDR", &header));
    let raw = [0, rgb[0], rgb[1], rgb[2]];
    let (mut a, mut b) = (1_u32, 0_u32);
    for &byte in &raw {
        a = (a + u32::from(byte)) % 65521;
        b = (b + a) % 65521;
    }
    let mut deflate = vec![0x78, 0x01, 0x01, 4, 0, 0xfb, 0xff];
    deflate.extend_from_slice(&raw);
    deflate.extend_from_slice(&((b << 16) | a).to_be_bytes());
    out.extend(png_chunk(*b"IDAT", &deflate));
    out.extend(png_chunk(*b"IEND", &[]));
    out
}

/// A PNG padded with an ancillary chunk to `len` bytes: still a PNG, but
/// as large as a test needs.
pub fn big_png(rgb: [u8; 3], len: usize) -> Vec<u8> {
    let base = png(rgb);
    let iend = base.len() - 12;
    let mut out = base[..iend].to_vec();
    let pad = len.saturating_sub(base.len() + 12);
    out.extend(png_chunk(*b"tEXt", &vec![b'x'; pad]));
    out.extend_from_slice(&base[iend..]);
    out
}

/// A zip with the given entries, stored.
pub fn zip(entries: &[(&str, Vec<u8>)]) -> Vec<u8> {
    let mut writer = zip::ZipWriter::new(std::io::Cursor::new(Vec::new()));
    let options =
        zip::write::SimpleFileOptions::default().compression_method(zip::CompressionMethod::Stored);
    for (name, body) in entries {
        writer.start_file(*name, options).expect("entry");
        writer.write_all(body).expect("body");
    }
    writer.finish().expect("finish").into_inner()
}

const OFFICE_DOCUMENT_REL: &str =
    "http://schemas.openxmlformats.org/officeDocument/2006/relationships";

fn root_rels(target: &str) -> Vec<u8> {
    format!(
        r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="{OFFICE_DOCUMENT_REL}/officeDocument" Target="{target}"/></Relationships>"#
    )
    .into_bytes()
}

fn image_rels(base: &str, images: &[(String, String)]) -> Vec<u8> {
    let rels: String = images
        .iter()
        .map(|(id, name)| {
            format!(
                r#"<Relationship Id="{id}" Type="{OFFICE_DOCUMENT_REL}/image" Target="{base}media/{name}"/>"#
            )
        })
        .collect();
    format!(
        r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">{rels}</Relationships>"#
    )
    .into_bytes()
}

/// One item of a DOCX body: a paragraph of text or a picture.
pub enum Block<'a> {
    Text(&'a str),
    /// A picture with this alt text; the bytes are the next of `images`.
    Picture(&'a str),
}

/// A DOCX whose body is `blocks` in order; each picture takes the next of
/// `images`.
pub fn docx_with(blocks: &[Block<'_>], images: &[Vec<u8>]) -> Vec<u8> {
    let mut next = 0;
    let mut rels = Vec::new();
    let body: String = blocks
        .iter()
        .map(|block| match block {
            Block::Text(text) => format!("<w:p><w:r><w:t>{text}</w:t></w:r></w:p>"),
            Block::Picture(alt) => {
                next += 1;
                let (id, name) = (format!("rId{}", 10 + next), format!("image{next}.png"));
                rels.push((id.clone(), name));
                format!(
                    r#"<w:p><w:r><w:drawing><wp:inline><wp:extent cx="952500" cy="952500"/><wp:docPr id="{next}" name="Picture {next}" descr="{alt}"/><a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture"><pic:pic><pic:nvPicPr><pic:cNvPr id="0" name="Picture {next}"/><pic:cNvPicPr/></pic:nvPicPr><pic:blipFill><a:blip r:embed="{id}"/></pic:blipFill></pic:pic></a:graphicData></a:graphic></wp:inline></w:drawing></w:r></w:p>"#
                )
            }
        })
        .collect();
    let mut entries: Vec<(String, Vec<u8>)> = vec![
        (
            "[Content_Types].xml".to_owned(),
            br#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Default Extension="png" ContentType="image/png"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>"#.to_vec(),
        ),
        ("_rels/.rels".to_owned(), root_rels("word/document.xml")),
        (
            "word/_rels/document.xml.rels".to_owned(),
            image_rels("", &rels),
        ),
        (
            "word/document.xml".to_owned(),
            format!(
                r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><w:body>{body}</w:body></w:document>"#
            )
            .into_bytes(),
        ),
    ];
    for (index, image) in images.iter().enumerate().take(next) {
        entries.push((format!("word/media/image{}.png", index + 1), image.clone()));
    }
    let borrowed: Vec<(&str, Vec<u8>)> = entries
        .iter()
        .map(|(name, body)| (name.as_str(), body.clone()))
        .collect();
    zip(&borrowed)
}

/// A PPTX: each slide has a head line, then optionally a picture (the next
/// of `images`), then a tail line.
pub fn pptx_with(slides: &[(&str, bool)], images: &[Vec<u8>]) -> Vec<u8> {
    let mut next = 0;
    let mut entries: Vec<(String, Vec<u8>)> = vec![
        (
            "[Content_Types].xml".to_owned(),
            format!(
                r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Default Extension="png" ContentType="image/png"/><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>{}</Types>"#,
                (1..=slides.len())
                    .map(|i| format!(
                        r#"<Override PartName="/ppt/slides/slide{i}.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.slide+xml"/>"#
                    ))
                    .collect::<String>()
            )
            .into_bytes(),
        ),
        ("_rels/.rels".to_owned(), root_rels("ppt/presentation.xml")),
        (
            "ppt/presentation.xml".to_owned(),
            format!(
                r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst>{}</p:sldIdLst></p:presentation>"#,
                (1..=slides.len())
                    .map(|i| format!(r#"<p:sldId id="{}" r:id="rId{i}"/>"#, 255 + i))
                    .collect::<String>()
            )
            .into_bytes(),
        ),
        (
            "ppt/_rels/presentation.xml.rels".to_owned(),
            format!(
                r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">{}</Relationships>"#,
                (1..=slides.len())
                    .map(|i| format!(
                        r#"<Relationship Id="rId{i}" Type="{OFFICE_DOCUMENT_REL}/slide" Target="slides/slide{i}.xml"/>"#
                    ))
                    .collect::<String>()
            )
            .into_bytes(),
        ),
    ];
    for (index, (text, has_picture)) in slides.iter().enumerate() {
        let slide_no = index + 1;
        let picture = if *has_picture {
            next += 1;
            entries.push((
                format!("ppt/slides/_rels/slide{slide_no}.xml.rels"),
                image_rels("../", &[("rId2".to_owned(), format!("image{next}.png"))]),
            ));
            entries.push((
                format!("ppt/media/image{next}.png"),
                images[next - 1].clone(),
            ));
            format!(
                r#"<p:pic><p:nvPicPr><p:cNvPr id="4" name="Picture {next}" descr=""/><p:cNvPicPr/><p:nvPr/></p:nvPicPr><p:blipFill><a:blip r:embed="rId2"/></p:blipFill><p:spPr><a:xfrm><a:off x="0" y="0"/><a:ext cx="100" cy="100"/></a:xfrm></p:spPr></p:pic>"#
            )
        } else {
            String::new()
        };
        // An empty text is a slide of its picture alone.
        let (head, tail) = if text.is_empty() {
            (String::new(), String::new())
        } else {
            (
                format!(
                    "<p:sp><p:txBody><a:p><a:r><a:t>{text}</a:t></a:r></a:p></p:txBody></p:sp>"
                ),
                format!(
                    "<p:sp><p:txBody><a:p><a:r><a:t>{text} tail</a:t></a:r></a:p></p:txBody></p:sp>"
                ),
            )
        };
        entries.push((
            format!("ppt/slides/slide{slide_no}.xml"),
            format!(
                r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?><p:sld xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:cSld><p:spTree>{head}{picture}{tail}</p:spTree></p:cSld></p:sld>"#
            )
            .into_bytes(),
        ));
    }
    let borrowed: Vec<(&str, Vec<u8>)> = entries
        .iter()
        .map(|(name, body)| (name.as_str(), body.clone()))
        .collect();
    zip(&borrowed)
}

/// A PDF: one page per entry (its text, then whether it shows a 2x2 RGB
/// picture drawn between two lines of that text).
pub fn pdf_with_pictures(pages: &[(&str, bool)]) -> Vec<u8> {
    let image_object = 3 + 2 * pages.len() + 1;
    let mut objects: Vec<String> = vec![
        "<< /Type /Catalog /Pages 2 0 R >>".to_owned(),
        format!(
            "<< /Type /Pages /Kids [{}] /Count {} >>",
            (0..pages.len())
                .map(|i| format!("{} 0 R", 4 + 2 * i))
                .collect::<Vec<_>>()
                .join(" "),
            pages.len()
        ),
        "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>".to_owned(),
    ];
    for (index, (text, has_picture)) in pages.iter().enumerate() {
        let xobject = if *has_picture {
            format!("/XObject << /Im1 {image_object} 0 R >>")
        } else {
            String::new()
        };
        objects.push(format!(
            "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> {xobject} >> /Contents {} 0 R >>",
            5 + 2 * index
        ));
        let picture = if *has_picture {
            "q 100 0 0 100 72 500 cm /Im1 Do Q "
        } else {
            ""
        };
        // An empty text is a page of its picture alone (a scan).
        let stream = if text.is_empty() {
            picture.to_owned()
        } else {
            format!(
                "BT /F1 12 Tf 72 720 Td ({text} before) Tj ET {picture}BT /F1 12 Tf 72 400 Td ({text} after) Tj ET"
            )
        };
        objects.push(format!(
            "<< /Length {} >>\nstream\n{stream}\nendstream",
            stream.len()
        ));
    }
    let data = "FF0000 00FF00 0000FF FFFF00>";
    objects.push(format!(
        "<< /Type /XObject /Subtype /Image /Width 2 /Height 2 /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /ASCIIHexDecode /Length {} >>\nstream\n{data}\nendstream",
        data.len()
    ));
    let mut out = b"%PDF-1.4\n".to_vec();
    let mut offsets = Vec::new();
    for (i, object) in objects.iter().enumerate() {
        offsets.push(out.len());
        out.extend_from_slice(format!("{} 0 obj\n{object}\nendobj\n", i + 1).as_bytes());
    }
    let xref = out.len();
    out.extend_from_slice(
        format!("xref\n0 {}\n0000000000 65535 f \n", objects.len() + 1).as_bytes(),
    );
    for offset in offsets {
        out.extend_from_slice(format!("{offset:010} 00000 n \n").as_bytes());
    }
    out.extend_from_slice(
        format!(
            "trailer\n<< /Size {} /Root 1 0 R >>\nstartxref\n{xref}\n%%EOF\n",
            objects.len() + 1
        )
        .as_bytes(),
    );
    out
}
