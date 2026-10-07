//! The wall clock as Python's `datetime.now().isoformat()` writes it (UTC,
//! microseconds): the stamps graph metadata carries.

use std::time::{SystemTime, UNIX_EPOCH};

/// `YYYY-MM-DDTHH:MM:SS.ffffff`, UTC.
#[must_use]
pub fn now_iso() -> String {
    let since = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default();
    iso(since.as_secs(), since.subsec_micros())
}

/// [`now_iso`] for a given instant (Howard Hinnant's `civil_from_days`).
#[must_use]
pub fn iso(seconds: u64, micros: u32) -> String {
    let days = i64::try_from(seconds / 86_400).unwrap_or(0);
    let rest = seconds % 86_400;
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z - era * 146_097;
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = yoe + era * 400 + i64::from(month <= 2);
    format!(
        "{year:04}-{month:02}-{day:02}T{:02}:{:02}:{:02}.{micros:06}",
        rest / 3600,
        rest / 60 % 60,
        rest % 60
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn instants_format_as_isoformat() {
        assert_eq!(iso(0, 0), "1970-01-01T00:00:00.000000");
        assert_eq!(iso(1_759_881_600, 42), "2025-10-08T00:00:00.000042");
        assert_eq!(iso(951_782_400 + 3661, 5), "2000-02-29T01:01:01.000005");
    }
}
