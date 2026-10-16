//! Bounded corpus replay for the fuzz targets (ADR-0032 verification item 2).
//!
//! The cargo-fuzz targets in `fuzz/` run the same entry points under
//! libFuzzer for 10 minutes at the release gate. Here, on every CI run, a
//! deterministic corpus is replayed through them: seeds (our own files and the
//! python-generated fixtures), the hostile shapes, and byte-level and
//! XML-level mutations of the seeds. A panic inside a library, a missed
//! deadline or a stuck permit panics the harness, i.e. fails this test.
#![allow(clippy::pedantic)]
mod common;
use common::*;
use elitea_office_core::testing::{fuzz_deck, fuzz_document, fuzz_gate, fuzz_workbook};

const REAL_PPTX: &[u8] =
    include_bytes!("../../../../services/elitea-main/internal/infra/extract/testdata/deck.pptx");
const REAL_DOCX: &[u8] =
    include_bytes!("../../../../services/elitea-main/internal/infra/extract/testdata/notes.docx");
const REAL_XLSX: &[u8] =
    include_bytes!("../../../../services/elitea-main/internal/infra/extract/testdata/sheet.xlsx");

struct Rng(u64);
impl Rng {
    fn next(&mut self) -> u64 {
        self.0 = self
            .0
            .wrapping_mul(6_364_136_223_846_793_005)
            .wrapping_add(1_442_695_040_888_963_407);
        self.0 >> 33
    }
    fn below(&mut self, n: usize) -> usize {
        (self.next() as usize) % n.max(1)
    }
}

/// Byte-level damage to the whole file.
fn mutate_bytes(seed: &[u8], rng: &mut Rng) -> Vec<u8> {
    let mut d = seed.to_vec();
    if d.is_empty() {
        return noise(rng.below(64), rng.next());
    }
    match rng.below(4) {
        0 => {
            for _ in 0..1 + rng.below(8) {
                let i = rng.below(d.len());
                d[i] ^= 1 << rng.below(8);
            }
        }
        1 => d.truncate(rng.below(d.len())),
        2 => {
            let a = rng.below(d.len());
            let b = (a + rng.below(64)).min(d.len());
            d.drain(a..b);
        }
        _ => {
            let a = rng.below(d.len());
            d.splice(a..a, (0..rng.below(32)).map(|_| rng.next() as u8));
        }
    }
    d
}

/// Damage inside one XML part of a valid archive, so the gate passes and the
/// library parses the result.
fn mutate_part(seed: &[u8], rng: &mut Rng) -> Vec<u8> {
    let Some(mut e) = try_entries(seed) else {
        return mutate_bytes(seed, rng);
    };
    let xml: Vec<usize> = e
        .iter()
        .enumerate()
        .filter(|(_, (n, _))| n.ends_with(".xml"))
        .map(|(i, _)| i)
        .collect();
    if xml.is_empty() {
        return mutate_bytes(seed, rng);
    }
    let i = xml[rng.below(xml.len())];
    let d = &mut e[i].1;
    if d.is_empty() {
        return seed.to_vec();
    }
    match rng.below(5) {
        0 => {
            let at = rng.below(d.len());
            d[at] ^= 1 << rng.below(8);
        }
        1 => d.truncate(rng.below(d.len())),
        2 => {
            // Drop the tag around a random position.
            let at = rng.below(d.len());
            let start = d[..=at].iter().rposition(|&b| b == b'<');
            let end = d[at..].iter().position(|&b| b == b'>');
            if let (Some(s), Some(t)) = (start, end) {
                d.drain(s..=at + t);
            }
        }
        3 => {
            // Make numbers huge.
            let s = String::from_utf8_lossy(d)
                .replace("=\"1", "=\"2147483647")
                .replace("<v>1", "<v>1e308");
            *d = s.into_bytes();
        }
        _ => {
            let at = rng.below(d.len());
            let junk: Vec<u8> = b"<a:p><a:r><a:t>x</a:t></a:r></a:p>".to_vec();
            d.splice(at..at, junk);
        }
    }
    zip_of(&e)
}

fn replay(seeds: &[(&str, Vec<u8>)], harness: fn(&[u8]), rounds: usize) {
    for (name, seed) in seeds {
        harness(seed);
        let mut rng = Rng(name.bytes().fold(17, |a, b| a * 31 + u64::from(b)));
        for _ in 0..rounds {
            harness(&mutate_bytes(seed, &mut rng));
            harness(&mutate_part(seed, &mut rng));
        }
    }
}

#[test]
fn deck_corpus_replay() {
    let g = good_deck();
    let seeds = [
        ("real", REAL_PPTX.to_vec()),
        ("own", g.clone()),
        ("deep", nested(&g, MAIN_PPTX, 200)),
    ];
    replay(&seeds, fuzz_deck, 25);
}

#[test]
fn document_corpus_replay() {
    let g = good_docx();
    let seeds = [
        ("real", REAL_DOCX.to_vec()),
        ("own", g.clone()),
        ("deep", nested(&g, MAIN_DOCX, 200)),
    ];
    replay(&seeds, fuzz_document, 25);
}

#[test]
fn workbook_corpus_replay() {
    let g = good_xlsx();
    let seeds = [
        ("real", REAL_XLSX.to_vec()),
        ("own", g.clone()),
        ("deep", nested(&g, MAIN_XLSX, 200)),
    ];
    replay(&seeds, fuzz_workbook, 25);
}

#[test]
fn gate_corpus_replay() {
    let hostile = [
        ("garbage", noise(2048, 1)),
        ("empty", Vec::new()),
        ("bomb", bomb(&good_docx(), MAIN_DOCX, 16)),
        ("lied", lie_about_sizes(&good_deck())),
        ("nest", nested(&good_xlsx(), MAIN_XLSX, 5000)),
    ];
    replay(&hostile, fuzz_gate, 25);
}
