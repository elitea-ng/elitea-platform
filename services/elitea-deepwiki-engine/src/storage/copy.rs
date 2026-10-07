//! `COPY ... FROM STDIN` in the text format.
//!
//! WHY TEXT, NOT BINARY: the binary format needs each column's wire type,
//! and `vector` has none sqlx knows without the pgvector codec per row; the
//! text format needs only escaping. It also writes a vector the way the
//! Python publisher sent it: the decimal text of each `float` (Python
//! `repr`, Rust `Display`: both the shortest text that reads back as the
//! same `f64`), which pgvector rounds to `float4`. Same text in, same
//! `float4` out, so dense distances match bit for bit.

use crate::storage::{Result, StorageError};
use sqlx::postgres::PgConnection;
use std::io::Write as _;

/// Send the buffer when it grows past this. Bounds memory per `COPY`, and
/// is large enough that the per-message overhead does not matter.
const FLUSH_BYTES: usize = 4 * 1024 * 1024;

/// Rows of one `COPY` statement, encoded and sent in chunks.
pub(crate) struct CopyWriter<'c> {
    copy: sqlx::postgres::PgCopyIn<&'c mut PgConnection>,
    buffer: Vec<u8>,
    at_row_start: bool,
    rows: u64,
}

impl<'c> CopyWriter<'c> {
    /// Start `statement` (a `COPY ... FROM STDIN` in the text format).
    pub(crate) async fn start(connection: &'c mut PgConnection, statement: &str) -> Result<Self> {
        Ok(Self {
            copy: connection.copy_in_raw(statement).await?,
            buffer: Vec::with_capacity(FLUSH_BYTES + 64 * 1024),
            at_row_start: true,
            rows: 0,
        })
    }

    fn separator(&mut self) {
        if self.at_row_start {
            self.at_row_start = false;
        } else {
            self.buffer.push(b'\t');
        }
    }

    /// A text column.
    ///
    /// NUL cannot be stored in PostgreSQL text. It is written as U+FFFD
    /// (deliberate difference): the Python publisher failed the whole
    /// publish on it ("PostgreSQL text fields cannot contain NUL"), and the
    /// elitea-platform corpus has a node with one (a PDF fixture). U+FFFD is
    /// not whitespace, so token counts and BM25 lengths do not change.
    pub(crate) fn text(&mut self, value: &str) {
        self.separator();
        for byte in value.bytes() {
            match byte {
                b'\0' => self.buffer.extend_from_slice("\u{fffd}".as_bytes()),
                b'\\' => self.buffer.extend_from_slice(b"\\\\"),
                b'\n' => self.buffer.extend_from_slice(b"\\n"),
                b'\r' => self.buffer.extend_from_slice(b"\\r"),
                b'\t' => self.buffer.extend_from_slice(b"\\t"),
                other => self.buffer.push(other),
            }
        }
    }

    /// A nullable text column.
    pub(crate) fn opt_text(&mut self, value: Option<&str>) {
        if let Some(value) = value {
            self.text(value);
        } else {
            self.null();
        }
    }

    /// `\N`.
    pub(crate) fn null(&mut self) {
        self.separator();
        self.buffer.extend_from_slice(b"\\N");
    }

    /// An integer column.
    pub(crate) fn int(&mut self, value: i64) {
        self.separator();
        // Writing into a Vec cannot fail.
        let _ = write!(self.buffer, "{value}");
    }

    /// A nullable integer column.
    pub(crate) fn opt_int(&mut self, value: Option<i64>) {
        match value {
            Some(value) => self.int(value),
            None => self.null(),
        }
    }

    /// A floating-point column, as its shortest round-trip decimal text.
    pub(crate) fn float(&mut self, value: f64, row: &str) -> Result<()> {
        self.separator();
        if !value.is_finite() {
            return Err(StorageError::Publish(format!(
                "{row}: a non-finite number ({value}) cannot be stored"
            )));
        }
        let _ = write!(self.buffer, "{value}");
        Ok(())
    }

    /// A boolean column.
    pub(crate) fn boolean(&mut self, value: bool) {
        self.separator();
        self.buffer.push(if value { b't' } else { b'f' });
    }

    /// A `vector` column: `[x,y,...]` in pgvector's text input form.
    pub(crate) fn vector(&mut self, values: &[f64], row: &str) -> Result<()> {
        self.separator();
        if values.is_empty() {
            return Err(StorageError::Publish(format!(
                "{row}: an empty embedding cannot be stored"
            )));
        }
        if values.iter().any(|value| !value.is_finite()) {
            return Err(StorageError::Publish(format!(
                "{row}: the embedding has a non-finite component"
            )));
        }
        self.buffer.push(b'[');
        for (index, value) in values.iter().enumerate() {
            if index > 0 {
                self.buffer.push(b',');
            }
            let _ = write!(self.buffer, "{value}");
        }
        self.buffer.push(b']');
        Ok(())
    }

    /// End the current row; send the buffer when it is large.
    pub(crate) async fn end_row(&mut self) -> Result<()> {
        self.buffer.push(b'\n');
        self.at_row_start = true;
        self.rows += 1;
        if self.buffer.len() >= FLUSH_BYTES {
            self.flush().await?;
        }
        Ok(())
    }

    async fn flush(&mut self) -> Result<()> {
        if !self.buffer.is_empty() {
            let chunk = std::mem::take(&mut self.buffer);
            self.copy.send(chunk).await?;
            self.buffer = Vec::with_capacity(FLUSH_BYTES + 64 * 1024);
        }
        Ok(())
    }

    /// Send what is left and end the `COPY`. Returns the rows written.
    pub(crate) async fn finish(mut self) -> Result<u64> {
        self.flush().await?;
        let rows = self.rows;
        self.copy.finish().await?;
        Ok(rows)
    }

    /// Abandon the `COPY` (the server discards every row of it).
    pub(crate) async fn abort(self, reason: &str) -> Result<()> {
        self.copy.abort(reason).await?;
        Ok(())
    }
}
