//! The wall clock as Python's `datetime.now().isoformat()` writes it (UTC,
//! microseconds): the stamps graph metadata carries.

/// `YYYY-MM-DDTHH:MM:SS.ffffff`, UTC.
#[must_use]
pub fn now_iso() -> String {
    jiff::Timestamp::now()
        .strftime("%Y-%m-%dT%H:%M:%S%.6f")
        .to_string()
}

#[cfg(test)]
mod tests {
    #[test]
    fn the_stamp_has_microseconds() {
        let stamp = super::now_iso();
        assert_eq!(stamp.len(), 26, "{stamp}");
        assert_eq!(&stamp[10..11], "T");
    }
}
