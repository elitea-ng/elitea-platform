use serde_json::Number;

use super::{Decimal, NumberFault, decimal, exact_i64, exact_u64};

/// The `Display` text of a parsed number, as a caller without
/// `arbitrary_precision` passes it (this workspace builds without it).
fn displayed(json: &str) -> String {
    serde_json::from_str::<Number>(json)
        .expect("number fixture")
        .to_string()
}

#[test]
fn integral_spellings_read_exactly_and_fractions_do_not() {
    for (text, value) in [
        ("0", 0),
        ("-0", 0),
        ("7", 7),
        ("2.0", 2),
        ("1e2", 100),
        ("1.50e1", 15),
        ("-12", -12),
    ] {
        assert_eq!(exact_i64(text), Ok(value), "{text}");
    }
    for text in ["2.5", "1e-1", "0.5"] {
        assert_eq!(exact_i64(text), Err(NumberFault::NotInteger), "{text}");
    }
    assert_eq!(exact_u64("-1"), Err(NumberFault::NotInteger));
    assert_eq!(exact_u64("18446744073709551615"), Ok(u64::MAX));
}

#[test]
fn bounds_overflow_and_unsupported_text_are_typed() {
    assert_eq!(exact_i64("9223372036854775807"), Ok(i64::MAX));
    assert_eq!(exact_i64("-9223372036854775808"), Ok(i64::MIN));
    assert_eq!(exact_i64("9223372036854775808"), Err(NumberFault::Overflow));
    assert_eq!(exact_i64("1e20"), Err(NumberFault::Overflow));
    assert_eq!(
        exact_i64("1e99999999999999999999"),
        Err(NumberFault::Unsupported)
    );
    for text in ["", "-", "1e", "1.2.3", "0x1", "NaN"] {
        assert_eq!(exact_i64(text), Err(NumberFault::Unsupported), "{text}");
    }
}

#[test]
fn decimal_form_is_unique() {
    let form = |text| decimal(text).expect("decimal");
    assert_eq!(form("100"), form("1e2"));
    assert_eq!(form("1.0"), form("1"));
    assert_eq!(
        form("0.00"),
        Decimal {
            negative: false,
            digits: String::new(),
            exponent: 0
        }
    );
    assert_eq!(
        form("-1.50"),
        Decimal {
            negative: true,
            digits: "15".to_owned(),
            exponent: -1
        }
    );
}

#[test]
fn display_text_reads_exactly_without_arbitrary_precision() {
    assert_eq!(exact_i64(&displayed("9223372036854775807")), Ok(i64::MAX));
    assert_eq!(exact_i64(&displayed("-9223372036854775808")), Ok(i64::MIN));
    assert_eq!(exact_u64(&displayed("18446744073709551615")), Ok(u64::MAX));
    assert_eq!(exact_i64(&displayed("2.0")), Ok(2));
    assert_eq!(exact_i64(&displayed("1e2")), Ok(100));
    assert_eq!(exact_i64(&displayed("2.5")), Err(NumberFault::NotInteger));
}
