//! Writing a dataset to the project's artifact bucket: the one place the
//! analysis toolkits store what they extract, through the same artifact host
//! the other tracker families use.

use crate::toolkits::families::attachment_host::AttachmentHost;

use super::csv::{self, CsvError};
use super::table::Table;

/// A dataset file that was written.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct Stored {
    pub(crate) bucket: String,
    pub(crate) key: String,
}

impl Stored {
    /// The artifact path other tools take: `/{bucket}/{file}`.
    pub(crate) fn path(&self) -> String {
        format!("/{}/{}", self.bucket, self.key)
    }
}

/// A dataset encoded as CSV and checked against the byte cap, ready to store.
/// Encoding every file of a result before storing the first means a bound that
/// is hit never leaves a file behind.
pub(crate) struct Prepared(Vec<u8>);

/// Encode `table` as CSV, refusing a file over `max_bytes`. A failure is the
/// sentence the model reads.
pub(crate) fn prepare(table: &Table, max_bytes: usize) -> Result<Prepared, String> {
    csv::write(table, max_bytes)
        .map(Prepared)
        .map_err(|error| match error {
            CsvError::TooLarge => format!(
                "The dataset is larger than {}, so nothing was written. Narrow the projects, the dates or the filter.",
                if max_bytes >= 1_024 * 1_024 {
                    format!("{} MiB", max_bytes / (1_024 * 1_024))
                } else {
                    format!("{max_bytes} bytes")
                }
            ),
            CsvError::TooManyRows | CsvError::Malformed => {
                "The dataset could not be written as CSV.".to_owned()
            }
        })
}

/// Store a prepared dataset as `{stem}.csv`, or `{stem}-2.csv`, ... when the
/// name is taken: an extraction never replaces a file. The bucket is created
/// when it does not exist. A failure is the sentence the model reads.
pub(crate) async fn store(
    host: &AttachmentHost,
    bucket: &str,
    stem: &str,
    prepared: Prepared,
) -> Result<Stored, String> {
    let stored = host
        .write_unique_artifact_creating_bucket(bucket, stem, "csv", "text/csv", prepared.0)
        .await
        .map_err(|error| error.sentence())?;
    Ok(Stored {
        bucket: stored.bucket,
        key: stored.key,
    })
}

/// [`prepare`] and [`store`] one dataset.
pub(crate) async fn write_dataset(
    host: &AttachmentHost,
    bucket: &str,
    stem: &str,
    table: &Table,
    max_bytes: usize,
) -> Result<Stored, String> {
    store(host, bucket, stem, prepare(table, max_bytes)?).await
}
