//! The floe attribution rule (ADR-0034 decision 2): every file that says
//! it is adapted from floe is listed in NOTICE, and every file NOTICE lists
//! says so in its header.
#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::collections::BTreeSet;
use std::path::Path;

const MARK: &str = "adapted from floe (MIT)";

fn walk(dir: &Path, root: &Path, out: &mut BTreeSet<String>) {
    for entry in std::fs::read_dir(dir).unwrap() {
        let path = entry.unwrap().path();
        if path.is_dir() {
            walk(&path, root, out);
        } else if path.extension().is_some_and(|ext| ext == "rs") {
            let text = std::fs::read_to_string(&path).unwrap();
            let header: String = text.lines().take(12).collect::<Vec<_>>().join("\n");
            if header.contains(MARK) {
                out.insert(
                    path.strip_prefix(root)
                        .unwrap()
                        .to_string_lossy()
                        .replace('\\', "/"),
                );
            }
        }
    }
}

#[test]
fn notice_lists_exactly_the_files_adapted_from_floe() {
    let root = Path::new(env!("CARGO_MANIFEST_DIR"));
    let mut marked = BTreeSet::new();
    walk(&root.join("src"), root, &mut marked);
    let notice = std::fs::read_to_string(root.join("NOTICE")).unwrap();
    assert!(
        notice.contains("MIT License"),
        "NOTICE carries floe's license"
    );
    let listed: BTreeSet<String> = notice
        .lines()
        .filter_map(|line| line.trim().strip_prefix("adapted: "))
        .map(|rest| rest.split_whitespace().next().unwrap().to_owned())
        .collect();
    assert!(
        !marked.is_empty(),
        "the check found no marked file: it would pass vacuously"
    );
    assert_eq!(
        marked, listed,
        "files marked `{MARK}` vs files NOTICE lists"
    );
}
