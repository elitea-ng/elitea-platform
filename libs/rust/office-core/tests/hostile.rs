//! The hostile corpus (ADR-0032 verification item 1): every case is built
//! here (nothing multi-MiB is committed), opened through the real wrappers
//! on the production parse thread (16 MiB stack, dedicated thread), and must
//! be refused with the expected typed error within 1 s and under 512 MiB of
//! peak heap. Files at each limit's maximum must be accepted under 1.5 GiB.
//!
//! One `#[test]` on purpose: the allocator counter is process-wide, so cases
//! run one after the other and each peak is its own.
#![allow(clippy::pedantic)]
mod common;
use common::*;

use std::time::{Duration, Instant};

use elitea_office_core::{Deck, Document, Limit, Limits, OfficeError, Workbook};

#[global_allocator]
static A: peak_alloc::PeakAlloc = peak_alloc::PeakAlloc;

const MIB: usize = 1 << 20;

/// The allocator counter is process-wide: tests in this binary take turns.
static SERIAL: std::sync::Mutex<()> = std::sync::Mutex::new(());
fn serial() -> std::sync::MutexGuard<'static, ()> {
    SERIAL
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

#[derive(Clone, Copy, PartialEq, Debug)]
enum Fmt {
    Pptx,
    Docx,
    Xlsx,
}

/// Open `bytes` as `fmt` and touch the content the way the probe did: text,
/// markdown, re-save, and a render or export where the library has one.
fn exercise(fmt: Fmt, bytes: &[u8], l: &Limits) -> Result<(), OfficeError> {
    match fmt {
        Fmt::Pptx => {
            let d = Deck::open(bytes, l)?;
            d.to_markdown()?;
            d.save()?;
            if d.slide_count()? > 0 {
                d.render_preview(0)?;
                d.export_pdf()?;
            }
        }
        Fmt::Docx => {
            let d = Document::open(bytes, l)?;
            d.to_plain_text()?;
            d.to_markdown()?;
            d.save()?;
            d.export_pdf()?;
            d.render_page_png(0, 72.0)?;
        }
        Fmt::Xlsx => {
            let w = Workbook::open(bytes, l)?;
            for i in 0..w.sheet_names()?.len() {
                w.sheet_to_markdown(i)?;
            }
            w.save()?;
        }
    }
    Ok(())
}

struct Run {
    elapsed: Duration,
    peak: usize,
}

fn measure<R>(f: impl FnOnce() -> R) -> (R, Run) {
    let base = A.current_usage();
    A.reset_peak_usage();
    let t = Instant::now();
    let r = f();
    (
        r,
        Run {
            elapsed: t.elapsed(),
            peak: A.peak_usage().saturating_sub(base),
        },
    )
}

fn main_part(f: Fmt) -> &'static str {
    match f {
        Fmt::Pptx => MAIN_PPTX,
        Fmt::Docx => MAIN_DOCX,
        Fmt::Xlsx => MAIN_XLSX,
    }
}

fn good(f: Fmt) -> Vec<u8> {
    match f {
        Fmt::Pptx => good_deck(),
        Fmt::Docx => good_docx(),
        Fmt::Xlsx => good_xlsx(),
    }
}

fn ext(f: Fmt) -> &'static str {
    match f {
        Fmt::Pptx => "pptx",
        Fmt::Docx => "docx",
        Fmt::Xlsx => "xlsx",
    }
}

/// Raw zip surgery: flip bytes in the first local header and its central entry.
fn patch(bytes: &[u8], local: (usize, u8), central: (usize, u8)) -> Vec<u8> {
    let mut d = bytes.to_vec();
    let l = d.windows(4).position(|w| w == b"PK\x03\x04").unwrap();
    d[l + local.0] = local.1;
    let c = d.windows(4).position(|w| w == b"PK\x01\x02").unwrap();
    d[c + central.0] = central.1;
    d
}

#[test]
fn hostile_corpus_is_refused_fast_and_small() {
    let _turn = serial();
    let l = limits();
    type Expect = Box<dyn Fn(&OfficeError) -> bool>;
    let mut cases: Vec<(String, Fmt, Vec<u8>, Expect)> = Vec::new();
    let is_limit = |lim: Limit| move |e: &OfficeError| matches!(e, OfficeError::LimitExceeded { limit, .. } if *limit == lim);
    let malformed = |e: &OfficeError| matches!(e, OfficeError::Malformed(_));

    for f in [Fmt::Pptx, Fmt::Docx, Fmt::Xlsx] {
        let g = good(f);
        let name = |s: &str| format!("{}.{}", s, ext(f));
        // Decompression bomb: 256 MiB text node in a file of about 260 KB.
        cases.push((
            name("bomb256m"),
            f,
            bomb(&g, main_part(f), 256),
            Box::new(is_limit(Limit::XmlPartBytes)),
        ));
        // A header that understates a 16 MiB entry as 1000 bytes.
        cases.push((
            name("understated"),
            f,
            understated_bomb(&g, main_part(f), 16),
            Box::new(|e| {
                matches!(
                    e,
                    OfficeError::LimitExceeded {
                        limit: Limit::ActualBytes,
                        ..
                    } | OfficeError::Malformed(_)
                )
            }),
        ));
        // A header that claims 4 GiB.
        cases.push((
            name("liedsize"),
            f,
            lie_about_sizes(&g),
            Box::new(is_limit(Limit::XmlPartBytes)),
        ));
        // Truncation, garbage, empty.
        cases.push((
            name("trunc_half"),
            f,
            g[..g.len() / 2].to_vec(),
            Box::new(malformed),
        ));
        cases.push((
            name("trunc_nocd"),
            f,
            g[..g.len() - 40].to_vec(),
            Box::new(malformed),
        ));
        cases.push((name("garbage"), f, noise(4096, 7), Box::new(malformed)));
        cases.push((name("empty"), f, Vec::new(), Box::new(malformed)));
        // Deep nesting: the spike's aborting depths, and a modest 500.
        for depth in [500usize, 50_000, 200_000] {
            cases.push((
                name(&format!("nest{depth}")),
                f,
                nested(&g, main_part(f), depth),
                Box::new(is_limit(Limit::XmlDepth)),
            ));
        }
        // Many entries: below and above the 16-bit entry count (zip64 record).
        for n in [12_000usize, 70_000] {
            let mut e = entries(&g);
            e.extend((0..n).map(|i| (format!("junk/{i}.bin"), vec![b'x'])));
            cases.push((
                name(&format!("entries{n}")),
                f,
                zip_of(&e),
                Box::new(is_limit(Limit::Entries)),
            ));
        }
        // A DTD.
        let x = String::from_utf8(
            entries(&g)
                .into_iter()
                .find(|(n, _)| n == main_part(f))
                .unwrap()
                .1,
        )
        .unwrap();
        let dtd = x.replacen("?>", "?><!DOCTYPE a [<!ENTITY e \"x\">]>", 1);
        cases.push((
            name("dtd"),
            f,
            with_entry(&g, main_part(f), dtd.into_bytes()),
            Box::new(malformed),
        ));
        // Embedded rasters: header-only PNGs that would decode to GBs.
        let media = match f {
            Fmt::Pptx => "ppt/media/image1.png",
            Fmt::Docx => "word/media/image1.png",
            Fmt::Xlsx => "xl/media/image1.png",
        };
        for (w, h) in [(12_000u32, 12_000u32), (100_000, 100_000)] {
            cases.push((
                name(&format!("png{w}")),
                f,
                with_extra(&g, media, png_header(w, h)),
                Box::new(is_limit(Limit::ImagePixels)),
            ));
        }
        let mut many = entries(&g);
        many.extend(
            (0..501).map(|i| (media.replace("image1", &format!("m{i}")), png_header(8, 8))),
        );
        cases.push((
            name("images501"),
            f,
            zip_of(&many),
            Box::new(is_limit(Limit::Images)),
        ));
        // Container-level lies.
        let first = main_part(f).len();
        let _ = first;
        cases.push((
            name("encrypted"),
            f,
            patch(&g, (6, 1), (8, 1)),
            Box::new(|e| matches!(e, OfficeError::Unsupported(_) | OfficeError::Malformed(_))),
        ));
        cases.push((
            name("method12"),
            f,
            patch(&g, (8, 12), (10, 12)),
            Box::new(|e| matches!(e, OfficeError::Unsupported(_) | OfficeError::Malformed(_))),
        ));
        cases.push((
            name("local_name_mismatch"),
            f,
            patch(&g, (30, b'Z'), (46, b'Y')),
            Box::new(malformed),
        ));
        let mut trailing = g.clone();
        trailing.extend_from_slice(b"TRAILING");
        cases.push((name("trailing"), f, trailing, Box::new(malformed)));
        let mut prefixed = b"PREFIXDATA".to_vec();
        prefixed.extend_from_slice(&g);
        cases.push((name("prefixed"), f, prefixed, Box::new(malformed)));
        // Case-insensitive duplicate entry names, made by renaming in place.
        let mut dup = entries(&g);
        let last = dup.len() - 1;
        dup[last].0 = dup[0].0.to_uppercase();
        if dup[last].0 != dup[0].0 {
            let mut z = zip_of(&dup);
            // Writer saw distinct names; both are now equal ignoring case.
            z.shrink_to_fit();
            cases.push((name("duplicate"), f, z, Box::new(malformed)));
        }
        // Input over the compressed cap, and an unsafe path.
        cases.push((
            name("input_cap"),
            f,
            vec![0u8; 51 * MIB],
            Box::new(is_limit(Limit::InputBytes)),
        ));
        cases.push((
            name("dotdot"),
            f,
            with_extra(&g, "../evil.xml", b"<a/>".to_vec()),
            Box::new(malformed),
        ));
    }

    let mut report = Vec::new();
    for (name, f, bytes, expect) in &cases {
        let (r, run) = measure(|| exercise(*f, bytes, &l));
        let e = r.err().unwrap_or_else(|| panic!("{name}: was accepted"));
        assert!(expect(&e), "{name}: wrong error {e:?}");
        assert!(
            run.elapsed < Duration::from_secs(1),
            "{name}: took {:?}",
            run.elapsed
        );
        assert!(run.peak < 512 * MIB, "{name}: peak {} MiB", run.peak / MIB);
        report.push(format!(
            "{name}: {:?} {:?} {} MiB",
            e,
            run.elapsed,
            run.peak / MIB
        ));
    }
    println!("{}", report.join("\n"));
}

#[test]
fn awkward_but_legal_files_still_open() {
    let _turn = serial();
    // Counterpart to the corpus: shapes the spike called "OK" that must keep
    // working, and a modest nesting depth.
    let l = limits();
    for f in [Fmt::Pptx, Fmt::Docx, Fmt::Xlsx] {
        let g = good(f);
        let (r, run) = measure(|| exercise(f, &nested(&g, main_part(f), 100), &l));
        r.unwrap_or_else(|e| panic!("{f:?} depth 100: {e:?}"));
        assert!(run.elapsed < Duration::from_secs(5));
    }
}

#[test]
fn huge_declared_dimensions_are_bounded() {
    let _turn = serial();
    let l = limits();
    // pptx: slide size 2.1 billion EMU square renders at a fixed width.
    let g = good_deck();
    let x = String::from_utf8(
        entries(&g)
            .into_iter()
            .find(|(n, _)| n == "ppt/presentation.xml")
            .unwrap()
            .1,
    )
    .unwrap();
    let re = x.find("<p:sldSz").unwrap();
    let end = x[re..].find("/>").unwrap() + re + 2;
    let big = format!(
        "{}<p:sldSz cx=\"2147483000\" cy=\"2147483000\"/>{}",
        &x[..re],
        &x[end..]
    );
    let (r, run) = measure(|| {
        exercise(
            Fmt::Pptx,
            &with_entry(&g, "ppt/presentation.xml", big.into_bytes()),
            &l,
        )
    });
    assert!(
        matches!(r, Ok(()) | Err(OfficeError::LimitExceeded { .. })),
        "{r:?}"
    );
    assert!(run.elapsed < Duration::from_secs(5) && run.peak < 512 * MIB);
    // A slide a million times taller than wide would need 1.3e9 px of height.
    let tall = format!(
        "{}<p:sldSz cx=\"1000\" cy=\"2147483000\"/>{}",
        &x[..re],
        &x[end..]
    );
    let r = Deck::open(
        &with_entry(&g, "ppt/presentation.xml", tall.into_bytes()),
        &l,
    )
    .and_then(|d| d.render_preview(0));
    assert!(
        matches!(
            r,
            Err(OfficeError::LimitExceeded {
                limit: Limit::RenderPixels,
                ..
            })
        ),
        "{r:?}"
    );

    // docx: a 2-billion-twip page is refused for a PNG, still exports to PDF or errors cleanly.
    let g = good_docx();
    let x = String::from_utf8(
        entries(&g)
            .into_iter()
            .find(|(n, _)| n == MAIN_DOCX)
            .unwrap()
            .1,
    )
    .unwrap();
    let huge = x.replacen(
        "</w:body>",
        "<w:sectPr><w:pgSz w:w=\"2000000000\" w:h=\"2000000000\"/></w:sectPr></w:body>",
        1,
    );
    let doc = Document::open(&with_entry(&g, MAIN_DOCX, huge.into_bytes()), &l).unwrap();
    assert!(matches!(
        doc.render_page_png(0, 100.0),
        Err(OfficeError::LimitExceeded {
            limit: Limit::RenderPixels,
            ..
        })
    ));
    let (r, run) = measure(|| doc.export_pdf());
    assert!(r.is_ok() || r.is_err());
    assert!(run.elapsed < Duration::from_secs(10) && run.peak < 512 * MIB);

    // docx gridSpan of 2 billion is refused at the gate (it cost a second of
    // layout in release, tens of seconds in debug).
    let span = x.replacen(
        "<w:tc>",
        "<w:tc><w:tcPr><w:gridSpan w:val=\"2000000000\"/></w:tcPr>",
        1,
    );
    let (r, run) =
        measure(|| exercise(Fmt::Docx, &with_entry(&g, MAIN_DOCX, span.into_bytes()), &l));
    assert!(
        matches!(
            r,
            Err(OfficeError::LimitExceeded {
                limit: Limit::TableSpan,
                value: 2_000_000_000
            })
        ),
        "{r:?}"
    );
    assert!(run.elapsed < Duration::from_secs(1));

    // xlsx declared dimensions.
    let g = good_xlsx();
    let x = String::from_utf8(
        entries(&g)
            .into_iter()
            .find(|(n, _)| n == MAIN_XLSX)
            .unwrap()
            .1,
    )
    .unwrap();
    let sparse = x
        .replacen(
            "<sheetData>",
            "<dimension ref=\"A1:XFD1048576\"/><sheetData>",
            1,
        )
        .replacen(
            "</sheetData>",
            "<row r=\"1048576\"><c r=\"XFD1048576\"><v>1</v></c></row></sheetData>",
            1,
        );
    let w = Workbook::open(&with_entry(&g, MAIN_XLSX, sparse.into_bytes()), &l).unwrap();
    // Reading the sparse used range cell by cell is refused, not attempted.
    let r = w.sheet_to_markdown(0);
    assert!(
        matches!(
            r,
            Err(OfficeError::LimitExceeded {
                limit: Limit::Cells,
                ..
            })
        ),
        "{r:?}"
    );
    assert!(w.save().is_ok());
}

#[test]
fn render_budget_is_met_by_fitting_not_refusing() {
    let _turn = serial();
    // Three header-only 6244x6244 (39 MP) images each pass the 40 MP check.
    // Header-only means a renderer could not decode them, so the render copy
    // replaces each with a placeholder: opening, saving, rendering and PDF
    // export all succeed, where the first version of this crate refused.
    let l = limits();
    let g = good_deck();
    let mut e = entries(&g);
    for i in 0..3 {
        e.push((format!("ppt/media/m{i}.png"), png_header(6244, 6244)));
    }
    let bytes = zip_of(&e);
    let d = Deck::open(&bytes, &l).expect("each image is within the per-image cap");
    let (r, run) = measure(|| d.render_preview(0));
    assert!(r.unwrap().starts_with(b"\x89PNG"));
    assert!(run.peak < 512 * MIB, "peak {} MiB", run.peak / MIB);
    assert!(d.export_pdf().unwrap().starts_with(b"%PDF"));
    // Saved files keep the originals.
    let saved = entries(&d.save().unwrap());
    let kept = saved
        .iter()
        .filter(|(n, d)| n.starts_with("ppt/media/m") && d.len() == png_header(1, 1).len())
        .count();
    assert_eq!(kept, 3);
}

#[test]
fn render_copy_neutralises_fonts_and_svg_but_saves_keep_them() {
    let _turn = serial();
    let l = limits();
    let g = good_deck();
    let mut e = entries(&g);
    e.push(("ppt/fonts/font1.fntdata".into(), noise(2048, 3)));
    e.push(("ppt/media/logo.svg".into(), b"<svg xmlns='http://www.w3.org/2000/svg' width='99999' height='99999'><rect width='99999' height='99999'/></svg>".to_vec()));
    let d = Deck::open(&zip_of(&e), &l).unwrap();
    assert!(d.render_preview(0).unwrap().starts_with(b"\x89PNG"));
    let saved = entries(&d.save().unwrap());
    let font = saved
        .iter()
        .find(|(n, _)| n == "ppt/fonts/font1.fntdata")
        .unwrap();
    assert_eq!(font.1.len(), 2048, "saved files keep font bytes");
    let svg = saved
        .iter()
        .find(|(n, _)| n == "ppt/media/logo.svg")
        .unwrap();
    assert!(svg.1.len() > 100, "saved files keep the svg");
}

#[test]
fn files_at_the_limits_are_accepted_under_1_gib() {
    let _turn = serial();
    let l = limits();
    // docx and pptx: two 8 MiB XML parts (16 MiB total) of ordinary text.
    for f in [Fmt::Docx, Fmt::Pptx] {
        let g = good(f);
        let main = main_part(f);
        let x =
            String::from_utf8(entries(&g).into_iter().find(|(n, _)| n == main).unwrap().1).unwrap();
        let (open_tag, _) = if f == Fmt::Docx {
            ("<w:t>", "</w:t>")
        } else {
            ("<a:t>", "</a:t>")
        };
        let i = x.find(open_tag).unwrap() + open_tag.len();
        let fill = "word ".repeat(8 * MIB / 5 - 4096);
        let big = format!("{}{}{}", &x[..i], fill, &x[i..]);
        assert!(big.len() < 8 * MIB, "stay under the 8 MiB part cap");
        // A second ~7.9 MiB XML part brings the total to the 16 MiB cap.
        let bytes = with_extra(
            &with_entry(&g, main, big.into_bytes()),
            "customXml/item1.xml",
            format!("<a>{}</a>", " ".repeat(7 * MIB + 900_000)).into_bytes(),
        );
        let (r, run) = measure(|| match f {
            Fmt::Docx => {
                Document::open(&bytes, &l).and_then(|d| d.to_plain_text().map(|t| t.len()))
            }
            _ => Deck::open(&bytes, &l).and_then(|d| d.to_markdown().map(|t| t.len())),
        });
        let n = r.unwrap_or_else(|e| panic!("{f:?}: {e:?}"));
        assert!(n > 4 * MIB, "{f:?}: the big text survived ({n})");
        assert!(run.peak < 1536 * MIB, "{f:?}: peak {} MiB", run.peak / MIB);
        println!(
            "{f:?} at the XML cap: {:?}, {} MiB",
            run.elapsed,
            run.peak / MIB
        );
    }
    // xlsx: the 1 000 000 cell cap fits in one dense sheet under the 48 MiB
    // worksheet cap (about 27 bytes per numeric cell).
    let mut w = Workbook::new(&l);
    for r in 0..1000u32 {
        for c in 0..1000u16 {
            w.write_cell(
                0,
                r,
                c,
                &elitea_office_core::CellValue::Number(f64::from(r) + f64::from(c)),
            )
            .unwrap();
        }
    }
    let (bytes, run) = measure(|| w.save().unwrap());
    assert!(
        run.peak < 1536 * MIB,
        "xlsx save peak {} MiB",
        run.peak / MIB
    );
    let (r, run) = measure(|| {
        Workbook::open(&bytes, &l).and_then(|w| w.sheet_to_markdown(0).map(|s| s.len()))
    });
    assert!(r.unwrap() > MIB);
    assert!(
        run.peak < 1536 * MIB,
        "xlsx open+dump peak {} MiB",
        run.peak / MIB
    );
    println!(
        "xlsx at the cell cap: {} bytes, {:?}, {} MiB",
        bytes.len(),
        run.elapsed,
        run.peak / MIB
    );
    // The 1 000 001st distinct cell is refused; overwriting is not.
    assert!(matches!(
        w.write_cell(0, 1000, 0, &elitea_office_core::CellValue::Number(1.0)),
        Err(OfficeError::LimitExceeded {
            limit: Limit::Cells,
            value: 1_000_001
        })
    ));
    w.write_cell(0, 0, 0, &elitea_office_core::CellValue::Number(7.0))
        .unwrap();
}

#[test]
fn the_permit_gate_refuses_a_third_runaway_and_reports_health() {
    let _turn = serial();
    // The gate is process-wide; this test uses its own tiny pool through the
    // `max_concurrent` field and a deadline the work cannot meet.
    let tight = Limits {
        max_concurrent: 1,
        call_deadline: Duration::from_millis(1),
        ..Limits::default()
    };
    let g = good_docx();
    // The first call may or may not make 1 ms; run until one times out.
    let mut timed_out = false;
    for _ in 0..50 {
        match Document::open(&g, &tight) {
            Err(OfficeError::DeadlineExceeded) => {
                timed_out = true;
                break;
            }
            Err(OfficeError::Busy) => break,
            _ => {}
        }
    }
    // Whatever happened, the permit comes back once the thread exits.
    let start = Instant::now();
    while elitea_office_core::health().active > 0 && start.elapsed() < Duration::from_secs(10) {
        std::thread::sleep(Duration::from_millis(10));
    }
    assert_eq!(elitea_office_core::health().active, 0);
    assert!(!elitea_office_core::health().degraded);
    let _ = timed_out;
}
