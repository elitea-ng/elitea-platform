//! The size caps of a handoff (ADR-0034 decision 5), as the begin response
//! sets them: enforced before anything is uploaded, with the largest paths
//! named so the person sees what to leave on the laptop.

use serde::{Deserialize, Serialize};

const MIB: u64 = 1024 * 1024;

/// The begin response's `limits` (contract §2.1).
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct Caps {
    pub max_segment_bytes: u64,
    pub max_file_bytes: u64,
    pub max_files: u64,
    pub max_unpacked_bytes: u64,
    /// What the project's cloud copies may still take (`None`: not known
    /// yet, before the begin call).
    #[serde(default)]
    pub project_workspace_bytes_left: Option<u64>,
}

impl Default for Caps {
    /// The ADR's defaults: 200 MB segments, 25 MB files, 20,000 files,
    /// 2 GB unpacked.
    fn default() -> Self {
        Self {
            max_segment_bytes: 200 * MIB,
            max_file_bytes: 25 * MIB,
            max_files: 20_000,
            max_unpacked_bytes: 2048 * MIB,
            project_workspace_bytes_left: None,
        }
    }
}

/// A cap a send exceeds.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct Violation {
    /// `max_file_bytes`, `max_files`, `max_unpacked_bytes`,
    /// `max_segment_bytes` or `project_workspace_bytes_left`.
    pub cap: String,
    pub limit: u64,
    pub actual: u64,
    /// For `max_file_bytes`: the file.
    #[serde(default)]
    pub path: Option<String>,
}

/// A path and its size.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct SizedPath {
    pub path: String,
    pub bytes: u64,
}

/// The caps a send exceeds, and its largest paths.
#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct CapReport {
    pub violations: Vec<Violation>,
    /// The largest files, largest first (at most [`LARGEST`]).
    pub largest: Vec<SizedPath>,
}

/// How many largest paths a report names.
pub const LARGEST: usize = 10;

impl CapReport {
    #[must_use]
    pub fn ok(&self) -> bool {
        self.violations.is_empty()
    }
}

/// Check the files a send transfers (and the files it would have had to
/// leave out for size, `oversize`) against `caps`.
#[must_use]
pub fn check(files: &[SizedPath], oversize: &[SizedPath], caps: &Caps) -> CapReport {
    let mut report = CapReport::default();
    for file in oversize {
        report.violations.push(Violation {
            cap: "max_file_bytes".into(),
            limit: caps.max_file_bytes,
            actual: file.bytes,
            path: Some(file.path.clone()),
        });
    }
    let count = (files.len() + oversize.len()) as u64;
    if count > caps.max_files {
        report.violations.push(Violation {
            cap: "max_files".into(),
            limit: caps.max_files,
            actual: count,
            path: None,
        });
    }
    let unpacked: u64 = files.iter().chain(oversize).map(|file| file.bytes).sum();
    if unpacked > caps.max_unpacked_bytes {
        report.violations.push(Violation {
            cap: "max_unpacked_bytes".into(),
            limit: caps.max_unpacked_bytes,
            actual: unpacked,
            path: None,
        });
    }
    let mut largest: Vec<SizedPath> = files.iter().chain(oversize).cloned().collect();
    largest.sort_by(|a, b| b.bytes.cmp(&a.bytes).then_with(|| a.path.cmp(&b.path)));
    largest.truncate(LARGEST);
    report.largest = largest;
    report
}

/// Check the segment itself (after the bundle is written).
pub fn check_segment(report: &mut CapReport, segment_bytes: u64, caps: &Caps) {
    if segment_bytes > caps.max_segment_bytes {
        report.violations.push(Violation {
            cap: "max_segment_bytes".into(),
            limit: caps.max_segment_bytes,
            actual: segment_bytes,
            path: None,
        });
    }
    if let Some(left) = caps.project_workspace_bytes_left
        && segment_bytes > left
    {
        report.violations.push(Violation {
            cap: "project_workspace_bytes_left".into(),
            limit: left,
            actual: segment_bytes,
            path: None,
        });
    }
}

#[cfg(test)]
mod tests {
    use super::{Caps, SizedPath, check, check_segment};

    fn sized(path: &str, bytes: u64) -> SizedPath {
        SizedPath {
            path: path.into(),
            bytes,
        }
    }

    #[test]
    fn every_cap_is_enforced_with_the_largest_paths() {
        let caps = Caps {
            max_segment_bytes: 100,
            max_file_bytes: 50,
            max_files: 3,
            max_unpacked_bytes: 120,
            project_workspace_bytes_left: Some(80),
        };
        let files = [sized("a", 40), sized("b", 45), sized("c", 10)];
        let oversize = [sized("big.bin", 60)];
        let mut report = check(&files, &oversize, &caps);
        check_segment(&mut report, 90, &caps);
        let caps_hit: Vec<&str> = report.violations.iter().map(|v| v.cap.as_str()).collect();
        assert_eq!(
            caps_hit,
            [
                "max_file_bytes",
                "max_files",
                "max_unpacked_bytes",
                "project_workspace_bytes_left"
            ]
        );
        assert_eq!(report.violations[0].path.as_deref(), Some("big.bin"));
        assert_eq!(report.largest[0].path, "big.bin");
        assert_eq!(report.largest[1].path, "b");
        assert!(!report.ok());
        let ok = check(&files, &[], &Caps::default());
        assert!(ok.ok());
        let mut segment = check(&files, &[], &caps);
        check_segment(&mut segment, 101, &caps);
        assert!(
            segment
                .violations
                .iter()
                .any(|v| v.cap == "max_segment_bytes")
        );
    }

    #[test]
    fn the_defaults_are_the_adrs() {
        let caps = Caps::default();
        assert_eq!(caps.max_segment_bytes, 209_715_200);
        assert_eq!(caps.max_file_bytes, 26_214_400);
        assert_eq!(caps.max_files, 20_000);
        assert_eq!(caps.max_unpacked_bytes, 2_147_483_648);
    }
}
