//! Disposable export cache. Exact bundle metadata remains the only content authority.
use super::{
    DependencyBundle, DependencyContentClient, DependencyContentError, open_verified_file,
};
use std::{
    collections::HashMap,
    sync::Arc,
    time::{Duration, Instant},
};

const ENTRY_LIMIT: usize = 4;
const RETENTION: Duration = Duration::from_mins(5);

#[derive(Default)]
pub(super) struct ExportCache {
    entries: HashMap<(String, usize), CachedExport>,
}
struct CachedExport {
    directory: Arc<tempfile::TempDir>,
    touched: Instant,
}
impl ExportCache {
    fn prune(&mut self) {
        self.entries
            .retain(|_, value| value.touched.elapsed() < RETENTION);
    }
}
impl DependencyContentClient {
    /// Reuse privately staged exports after a short-lived grant expires.
    /// # Errors
    /// Rejects an invalid index or unavailable cache ownership.
    pub fn cached_file(
        &self,
        bundle: &DependencyBundle,
        index: usize,
    ) -> Result<Option<Arc<tempfile::TempDir>>, DependencyContentError> {
        if index > bundle.file_count() {
            return Err(DependencyContentError::Integrity);
        }
        let mut cache = self
            .exports
            .lock()
            .map_err(|_| DependencyContentError::Configuration)?;
        cache.prune();
        Ok(cache
            .entries
            .get_mut(&(bundle.root().to_owned(), index))
            .map(|entry| {
                entry.touched = Instant::now();
                Arc::clone(&entry.directory)
            }))
    }

    /// Retain a verified export for a fresh-grant retry, within fixed storage bounds.
    /// At most four cached files consume 128 MiB. Active owners retain their own handles.
    /// # Errors
    /// Rejects changed files, invalid indexes, unavailable cache ownership, or full capacity.
    pub async fn cache_file(
        &self,
        bundle: &DependencyBundle,
        index: usize,
        directory: tempfile::TempDir,
    ) -> Result<Arc<tempfile::TempDir>, DependencyContentError> {
        if index > bundle.file_count() {
            return Err(DependencyContentError::Integrity);
        }
        if let Some(file) = bundle.files().get(index) {
            let _verified = open_verified_file(directory.path(), file).await?;
        }
        let mut cache = self
            .exports
            .lock()
            .map_err(|_| DependencyContentError::Configuration)?;
        cache.prune();
        let key = (bundle.root().to_owned(), index);
        if let Some(entry) = cache.entries.get_mut(&key) {
            entry.touched = Instant::now();
            return Ok(Arc::clone(&entry.directory));
        }
        if cache.entries.len() >= ENTRY_LIMIT {
            return Err(DependencyContentError::Busy);
        }
        let directory = Arc::new(directory);
        cache.entries.insert(
            key,
            CachedExport {
                directory: Arc::clone(&directory),
                touched: Instant::now(),
            },
        );
        Ok(directory)
    }

    pub(super) fn discard_export(&self, bundle: &DependencyBundle, index: usize) {
        if let Ok(mut cache) = self.exports.lock() {
            cache.entries.remove(&(bundle.root().to_owned(), index));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn expired_cache_drops_files_after_active_owner_releases() {
        let directory = Arc::new(tempfile::tempdir().unwrap());
        let path = directory.path().to_owned();
        let mut cache = ExportCache::default();
        cache.entries.insert(
            ("root".into(), 0),
            CachedExport {
                directory: Arc::clone(&directory),
                touched: Instant::now().checked_sub(RETENTION).unwrap(),
            },
        );
        cache.prune();
        assert!(cache.entries.is_empty());
        assert!(path.exists());
        drop(directory);
        assert!(!path.exists());
    }
}
