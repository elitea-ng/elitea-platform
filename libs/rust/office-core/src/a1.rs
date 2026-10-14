//! A1-style cell references, 0-based inside, 1-based outside.

use crate::workbook::CellRange;

/// The last row index (0-based) of a sheet.
pub(crate) const MAX_ROW: u32 = 1_048_575;
/// The last column index (0-based) of a sheet.
pub(crate) const MAX_COL: u16 = 16_383;

/// The column name of a 0-based column (`0` is `A`, `26` is `AA`).
#[must_use]
pub fn column_name(col: u16) -> String {
    let mut n = u32::from(col) + 1;
    let mut out = Vec::new();
    while n > 0 {
        n -= 1;
        // `n % 26` is below 26, so the narrowing is exact.
        out.push(b'A' + u8::try_from(n % 26).unwrap_or(0));
        n /= 26;
    }
    out.reverse();
    String::from_utf8(out).unwrap_or_default()
}

/// `B7` for row 6, column 1 (both 0-based).
#[must_use]
pub fn cell_ref(row: u32, col: u16) -> String {
    format!("{}{}", column_name(col), u64::from(row) + 1)
}

/// A cell reference such as `B7` or `$B$7` as `(row, col)`, 0-based. `None`
/// outside the sheet grid or for anything else.
#[must_use]
pub fn parse_cell_ref(text: &str) -> Option<(u32, u16)> {
    let t = text.trim().replace('$', "");
    let split = t.find(|c: char| c.is_ascii_digit())?;
    let (letters, digits) = t.split_at(split);
    if letters.is_empty()
        || letters.len() > 3
        || !letters.bytes().all(|b| b.is_ascii_alphabetic())
        || !digits.bytes().all(|b| b.is_ascii_digit())
    {
        return None;
    }
    let mut col = 0u32;
    for b in letters.bytes() {
        col = col * 26 + u32::from(b.to_ascii_uppercase() - b'A') + 1;
    }
    let col = u16::try_from(col - 1).ok().filter(|c| *c <= MAX_COL)?;
    let row: u32 = digits.parse().ok()?;
    let row = row.checked_sub(1).filter(|r| *r <= MAX_ROW)?;
    Some((row, col))
}

/// A range such as `A1:C5`, or a single cell `B2` (a one-cell range), 0-based.
/// The corners may come in either order.
#[must_use]
pub fn parse_range(text: &str) -> Option<CellRange> {
    let (a, b) = if let Some((a, b)) = text.split_once(':') {
        (parse_cell_ref(a)?, parse_cell_ref(b)?)
    } else {
        let c = parse_cell_ref(text)?;
        (c, c)
    };
    Some(CellRange {
        first_row: a.0.min(b.0),
        first_col: a.1.min(b.1),
        last_row: a.0.max(b.0),
        last_col: a.1.max(b.1),
    })
}

/// The `A1:C5` form of a range.
#[must_use]
pub fn range_ref(range: CellRange) -> String {
    let a = cell_ref(range.first_row, range.first_col);
    if range.first_row == range.last_row && range.first_col == range.last_col {
        a
    } else {
        format!("{a}:{}", cell_ref(range.last_row, range.last_col))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn columns_round_trip() {
        for col in [0u16, 1, 25, 26, 27, 701, 702, 16_383] {
            let name = column_name(col);
            assert_eq!(
                parse_cell_ref(&format!("{name}1")),
                Some((0, col)),
                "{name}"
            );
        }
        assert_eq!(column_name(0), "A");
        assert_eq!(column_name(26), "AA");
        assert_eq!(column_name(16_383), "XFD");
    }

    #[test]
    fn refs_and_ranges() {
        assert_eq!(parse_cell_ref("$B$7"), Some((6, 1)));
        assert_eq!(parse_cell_ref("XFD1048576"), Some((1_048_575, 16_383)));
        assert_eq!(parse_cell_ref("XFE1"), None);
        assert_eq!(parse_cell_ref("A1048577"), None);
        assert_eq!(parse_cell_ref("A0"), None);
        assert_eq!(parse_cell_ref("7B"), None);
        assert_eq!(parse_cell_ref(""), None);
        let r = parse_range("C5:A1").expect("range");
        assert_eq!(
            (r.first_row, r.first_col, r.last_row, r.last_col),
            (0, 0, 4, 2)
        );
        assert_eq!(range_ref(r), "A1:C5");
        assert_eq!(range_ref(parse_range("B2").expect("cell")), "B2");
        assert_eq!(parse_range("A1:"), None);
    }
}
