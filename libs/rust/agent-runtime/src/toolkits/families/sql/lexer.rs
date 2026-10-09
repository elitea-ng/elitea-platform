use super::config::SqlDialect;

pub(crate) const MAX_SQL_BYTES: usize = 64 * 1_024;
const MAX_DOLLAR_TAG_BYTES: usize = 63;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SqlLexError {
    Invalid,
    ResourceExhausted,
    MultipleStatements,
    TransactionControl,
}

/// A validated single statement. The original bytes are retained unchanged.
pub(crate) struct AdmittedSql<'a> {
    sql: &'a str,
}

impl<'a> AdmittedSql<'a> {
    #[must_use]
    pub(crate) const fn as_str(&self) -> &'a str {
        self.sql
    }
}

/// Admit exactly one SQL statement with dialect-aware comments and quoting.
///
/// This scanner is deliberately lexical; authorization never depends on its
/// statement classification. A single terminal semicolon is allowed. Explicit
/// transaction/session control is rejected because the driver owns the
/// transaction and closes the connection after each operation.
pub(crate) fn admit_one_statement(
    sql: &str,
    dialect: SqlDialect,
) -> Result<AdmittedSql<'_>, SqlLexError> {
    if sql.len() > MAX_SQL_BYTES {
        return Err(SqlLexError::ResourceExhausted);
    }
    if sql.is_empty()
        || sql
            .bytes()
            .any(|byte| byte == 0 || (byte.is_ascii_control() && !b"\t\n\r".contains(&byte)))
    {
        return Err(SqlLexError::Invalid);
    }

    let bytes = sql.as_bytes();
    let mut cursor = 0;
    let mut has_token = false;
    let mut terminal_semicolon = false;
    let mut first_keyword: Option<&str> = None;

    while cursor < bytes.len() {
        let byte = bytes[cursor];
        if byte.is_ascii_whitespace() {
            cursor += 1;
            continue;
        }
        if starts_with(bytes, cursor, b"--") {
            cursor = skip_line_comment(bytes, cursor + 2);
            continue;
        }
        if dialect == SqlDialect::MySql && byte == b'#' {
            cursor = skip_line_comment(bytes, cursor + 1);
            continue;
        }
        if starts_with(bytes, cursor, b"/*") {
            if bytes
                .get(cursor + 2)
                .is_some_and(|next| matches!(next, b'!' | b'+'))
            {
                return Err(SqlLexError::Invalid);
            }
            cursor = skip_block_comment(bytes, cursor + 2, dialect)?;
            continue;
        }
        if terminal_semicolon {
            return Err(SqlLexError::MultipleStatements);
        }
        if byte == b';' {
            if !has_token {
                return Err(SqlLexError::Invalid);
            }
            terminal_semicolon = true;
            cursor += 1;
            continue;
        }
        if dialect == SqlDialect::Postgres
            && matches!(byte, b'e' | b'E')
            && bytes.get(cursor + 1) == Some(&b'\'')
        {
            cursor = skip_quoted(bytes, cursor + 2, b'\'', BackslashMode::Escape)?;
            has_token = true;
            continue;
        }
        if dialect == SqlDialect::Postgres
            && matches!(byte, b'u' | b'U')
            && bytes.get(cursor + 1) == Some(&b'&')
            && bytes.get(cursor + 2) == Some(&b'"')
        {
            cursor = skip_quoted(bytes, cursor + 3, b'"', BackslashMode::Escape)?;
            has_token = true;
            continue;
        }
        if byte == b'\'' {
            let backslash = if dialect == SqlDialect::MySql {
                BackslashMode::Escape
            } else {
                BackslashMode::Literal
            };
            cursor = skip_quoted(bytes, cursor + 1, b'\'', backslash)?;
            has_token = true;
            continue;
        }
        if byte == b'"' {
            let backslash = if dialect == SqlDialect::MySql {
                BackslashMode::Escape
            } else {
                BackslashMode::Literal
            };
            cursor = skip_quoted(bytes, cursor + 1, b'"', backslash)?;
            has_token = true;
            continue;
        }
        if dialect == SqlDialect::MySql && byte == b'`' {
            cursor = skip_quoted(bytes, cursor + 1, b'`', BackslashMode::Escape)?;
            has_token = true;
            continue;
        }
        if dialect == SqlDialect::Postgres
            && byte == b'$'
            && let Some((delimiter, next)) = dollar_delimiter(bytes, cursor)?
        {
            cursor = skip_dollar_quoted(bytes, next, delimiter)?;
            has_token = true;
            continue;
        }

        if is_identifier_start(byte) {
            let start = cursor;
            cursor += 1;
            while cursor < bytes.len() && is_identifier_continue(bytes[cursor]) {
                cursor += 1;
            }
            if first_keyword.is_none() {
                first_keyword = Some(&sql[start..cursor]);
            }
            has_token = true;
            continue;
        }

        // Operators, punctuation, and numeric literals are part of this one
        // statement. Non-ASCII bytes are permitted in values/identifiers and
        // remain bounded by the UTF-8 Rust string and total-input limit.
        has_token = true;
        cursor += 1;
    }

    if !has_token {
        return Err(SqlLexError::Invalid);
    }
    if first_keyword.is_some_and(is_transaction_or_session_control) {
        return Err(SqlLexError::TransactionControl);
    }
    Ok(AdmittedSql { sql })
}

fn skip_line_comment(bytes: &[u8], mut cursor: usize) -> usize {
    while cursor < bytes.len() && !matches!(bytes[cursor], b'\n' | b'\r') {
        cursor += 1;
    }
    cursor
}

fn skip_block_comment(
    bytes: &[u8],
    mut cursor: usize,
    dialect: SqlDialect,
) -> Result<usize, SqlLexError> {
    let mut depth = 1usize;
    while cursor < bytes.len() {
        if starts_with(bytes, cursor, b"*/") {
            depth -= 1;
            cursor += 2;
            if depth == 0 {
                return Ok(cursor);
            }
            continue;
        }
        if dialect == SqlDialect::Postgres && starts_with(bytes, cursor, b"/*") {
            depth = depth.checked_add(1).ok_or(SqlLexError::Invalid)?;
            cursor += 2;
            continue;
        }
        cursor += 1;
    }
    Err(SqlLexError::Invalid)
}

#[derive(Clone, Copy, Eq, PartialEq)]
enum BackslashMode {
    Literal,
    Escape,
}

fn skip_quoted(
    bytes: &[u8],
    mut cursor: usize,
    quote: u8,
    backslash: BackslashMode,
) -> Result<usize, SqlLexError> {
    while cursor < bytes.len() {
        match bytes[cursor] {
            b'\\' if backslash == BackslashMode::Escape => {
                if cursor + 1 >= bytes.len() {
                    return Err(SqlLexError::Invalid);
                }
                cursor += 2;
            }
            byte if byte == quote => {
                if bytes.get(cursor + 1) == Some(&quote) {
                    cursor += 2;
                } else {
                    return Ok(cursor + 1);
                }
            }
            _ => cursor += 1,
        }
    }
    Err(SqlLexError::Invalid)
}

fn dollar_delimiter(bytes: &[u8], start: usize) -> Result<Option<(&[u8], usize)>, SqlLexError> {
    let mut cursor = start + 1;
    if bytes.get(cursor) == Some(&b'$') {
        return Ok(Some((&bytes[start..=cursor], cursor + 1)));
    }
    if !bytes
        .get(cursor)
        .is_some_and(|byte| is_identifier_start(*byte))
    {
        return Ok(None);
    }
    cursor += 1;
    while bytes
        .get(cursor)
        .is_some_and(|byte| byte.is_ascii_alphanumeric() || *byte == b'_')
    {
        if cursor - start > MAX_DOLLAR_TAG_BYTES {
            return Err(SqlLexError::ResourceExhausted);
        }
        cursor += 1;
    }
    if bytes.get(cursor) != Some(&b'$') {
        return Ok(None);
    }
    Ok(Some((&bytes[start..=cursor], cursor + 1)))
}

fn skip_dollar_quoted(
    bytes: &[u8],
    mut cursor: usize,
    delimiter: &[u8],
) -> Result<usize, SqlLexError> {
    while cursor < bytes.len() {
        if bytes[cursor..].starts_with(delimiter) {
            return Ok(cursor + delimiter.len());
        }
        cursor += 1;
    }
    Err(SqlLexError::Invalid)
}

fn is_transaction_or_session_control(value: &str) -> bool {
    matches!(
        value.to_ascii_lowercase().as_str(),
        "begin"
            | "commit"
            | "rollback"
            | "savepoint"
            | "release"
            | "start"
            | "set"
            | "reset"
            | "use"
            | "lock"
            | "unlock"
            | "prepare"
            | "deallocate"
    )
}

const fn is_identifier_start(byte: u8) -> bool {
    byte.is_ascii_alphabetic() || matches!(byte, b'_' | b'@')
}

const fn is_identifier_continue(byte: u8) -> bool {
    is_identifier_start(byte) || byte.is_ascii_digit() || byte == b'$'
}

fn starts_with(bytes: &[u8], cursor: usize, needle: &[u8]) -> bool {
    bytes
        .get(cursor..)
        .is_some_and(|rest| rest.starts_with(needle))
}
