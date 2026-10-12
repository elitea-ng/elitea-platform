//! Execution-bound encrypted crash-recovery spool.
//!
//! This module is intentionally synchronous. Async delivery code must invoke
//! it through a bounded blocking boundary so filesystem durability never
//! blocks the executor. The on-disk format remains compatible with the current
//! Python worker, while directory-relative operations and an advisory owner
//! lock close unsafe path and multi-process races.

use std::ffi::CStr;
use std::fmt;
use std::fs::File;
use std::io::{self, Read, Write};
use std::num::NonZeroU64;
use std::path::Path;

use ring::aead::{self, Aad, LessSafeKey, Nonce, UnboundKey};
use ring::digest;
use ring::hkdf;
use ring::rand::{SecureRandom, SystemRandom};
use rustix::fs::{
    AtFlags, Dir, FileType, FlockOperation, Mode, OFlags, flock, fstat, fsync, linkat, mkdirat,
    openat, renameat, statat, unlinkat,
};
use rustix::io::Errno;
use rustix::process::geteuid;
use zeroize::Zeroize;

const BINDING_DOMAIN: &[u8] = b"elitea.runtime.execution-spool.v1\0";
const KEY_DOMAIN: &[u8] = b"elitea.runtime.output-spool-key.v1\0";
const AAD_DOMAIN: &[u8] = b"elitea.runtime.output-spool-aad.v1\0";
const FILE_MAGIC: &[u8] = b"ELITEASPOOL1\0";
const NONCE_BYTES: usize = 12;
const TAG_BYTES: usize = 16;
/// Retired frames one execution spool keeps as records.
const MAX_RETIRED_FRAMES: usize = 16;
const FILE_OVERHEAD_BYTES: usize = FILE_MAGIC.len() + NONCE_BYTES + TAG_BYTES;
const MAX_IDENTITY_BYTES: usize = 256;
const TEMP_CREATE_ATTEMPTS: usize = 8;
const MAX_CONFIGURED_FRAMES: usize = 128;
const MAX_CONFIGURED_ENCRYPTED_BYTES: u64 = 64 * 1024 * 1024 + 128 * 64;
const MAX_CONFIGURED_FRAME_BYTES: usize = 64 * 1024;

/// Stable, data-free spool errors.
#[derive(Debug)]
pub enum SpoolError {
    InvalidInput(&'static str),
    ResourceExhausted(&'static str),
    OwnershipUnavailable(&'static str),
    Unavailable {
        message: &'static str,
        source: io::Error,
    },
}

impl fmt::Display for SpoolError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidInput(message)
            | Self::ResourceExhausted(message)
            | Self::OwnershipUnavailable(message)
            | Self::Unavailable { message, .. } => formatter.write_str(message),
        }
    }
}

impl std::error::Error for SpoolError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Unavailable { source, .. } => Some(source),
            Self::InvalidInput(_) | Self::ResourceExhausted(_) | Self::OwnershipUnavailable(_) => {
                None
            }
        }
    }
}

/// Owner-held 256-bit root key used only to derive execution-specific keys.
pub struct SpoolMasterKey([u8; 32]);

impl SpoolMasterKey {
    #[must_use]
    pub const fn new(value: [u8; 32]) -> Self {
        Self(value)
    }
}

impl Drop for SpoolMasterKey {
    fn drop(&mut self) {
        self.0.zeroize();
    }
}

/// Exact identity fields which isolate one execution spool.
#[derive(Debug, Eq, PartialEq)]
pub struct ExecutionSpoolIdentity {
    pub tenant_id: String,
    pub resource_project_id: String,
    pub projection_project_id: String,
    pub command_id: String,
    pub execution_id: String,
    pub generation: u64,
    pub producer_id: String,
}

/// Canonically encoded execution identity used for path and key derivation.
pub struct ExecutionSpoolBinding {
    encoded: Vec<u8>,
    directory_name: String,
}

impl ExecutionSpoolBinding {
    /// Encode the exact cross-language execution binding.
    ///
    /// # Errors
    ///
    /// Returns [`SpoolError::InvalidInput`] for an empty or oversized identity
    /// component or generation zero. Length-prefixing safely preserves every
    /// string already admitted by the current command contract.
    pub fn new(identity: &ExecutionSpoolIdentity) -> Result<Self, SpoolError> {
        if identity.generation == 0 {
            return Err(SpoolError::InvalidInput(
                "the execution spool identity is malformed",
            ));
        }
        let generation = identity.generation.to_string();
        let components = [
            identity.tenant_id.as_str(),
            identity.resource_project_id.as_str(),
            identity.projection_project_id.as_str(),
            identity.command_id.as_str(),
            identity.execution_id.as_str(),
            generation.as_str(),
            identity.producer_id.as_str(),
        ];
        if components
            .iter()
            .any(|component| !valid_identity(component))
        {
            return Err(SpoolError::InvalidInput(
                "the execution spool identity is malformed",
            ));
        }

        let encoded = encode_binding(&components)?;
        let directory_name = lower_hex(digest::digest(&digest::SHA256, &encoded).as_ref());
        Ok(Self {
            encoded,
            directory_name,
        })
    }

    /// Return the non-sensitive SHA-256 child directory name.
    #[must_use]
    pub fn directory_name(&self) -> &str {
        &self.directory_name
    }
}

/// Ciphertext capacity limits for one execution spool.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SpoolLimits {
    pub max_frames: usize,
    pub max_encrypted_bytes: u64,
    pub max_frame_bytes: usize,
}

/// One authenticated plaintext frame restored in sequence order.
#[derive(Eq, PartialEq)]
pub struct SpooledFrame {
    pub sequence: NonZeroU64,
    pub payload: Vec<u8>,
}

/// Single-owner handle for one execution-bound encrypted directory.
pub struct EncryptedOutputSpool {
    root: File,
    directory: File,
    child_name: String,
    cipher: LessSafeKey,
    stream_aad: Vec<u8>,
    limits: SpoolLimits,
    random: SystemRandom,
}

impl EncryptedOutputSpool {
    /// Open one spool beneath an existing canonical owner-private replica root.
    ///
    /// The returned handle holds a nonblocking exclusive advisory lock on the
    /// derived child directory. Deployment must still provide a distinct,
    /// local/block-backed root per worker replica; the current Python worker
    /// does not participate in this lock.
    ///
    /// # Errors
    ///
    /// Returns a bounded [`SpoolError`] for unsafe paths, invalid limits,
    /// another live owner, unsupported filesystem operations, or corrupt
    /// residue.
    pub fn open(
        configured_root: &Path,
        master_key: &SpoolMasterKey,
        binding: &ExecutionSpoolBinding,
        limits: SpoolLimits,
    ) -> Result<Self, SpoolError> {
        validate_limits(limits)?;
        let canonical = configured_root
            .canonicalize()
            .map_err(|error| unavailable("the output spool root is unavailable", error))?;
        if !configured_root.is_absolute() || canonical != configured_root {
            return Err(SpoolError::InvalidInput(
                "the output spool root is not canonical",
            ));
        }

        let root = File::from(
            openat(
                rustix::fs::CWD,
                configured_root,
                OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
                Mode::empty(),
            )
            .map_err(|error| unavailable_errno("the output spool root is unavailable", error))?,
        );
        validate_private_directory(&root, "the output spool root is unsafe")?;

        let created = match mkdirat(&root, binding.directory_name(), Mode::RWXU) {
            Ok(()) => true,
            Err(Errno::EXIST) => false,
            Err(error) => {
                return Err(unavailable_errno(
                    "the execution output spool is unavailable",
                    error,
                ));
            }
        };
        if created {
            sync_directory(&root)?;
        }

        let directory = File::from(
            openat(
                &root,
                binding.directory_name(),
                OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
                Mode::empty(),
            )
            .map_err(|error| {
                unavailable_errno("the execution output spool is unavailable", error)
            })?,
        );
        validate_private_directory(&directory, "the execution output spool is unsafe")?;
        match flock(&directory, FlockOperation::NonBlockingLockExclusive) {
            Ok(()) => {}
            Err(Errno::WOULDBLOCK) => {
                return Err(SpoolError::OwnershipUnavailable(
                    "the execution output spool already has a live owner",
                ));
            }
            Err(error) => {
                return Err(unavailable_errno(
                    "the execution output spool lock is unavailable",
                    error,
                ));
            }
        }

        let cipher = derive_cipher(master_key, &binding.encoded)?;
        let mut stream_aad = Vec::with_capacity(AAD_DOMAIN.len() + binding.encoded.len());
        stream_aad.extend_from_slice(AAD_DOMAIN);
        stream_aad.extend_from_slice(&binding.encoded);
        let mut spool = Self {
            root,
            directory,
            child_name: binding.directory_name.clone(),
            cipher,
            stream_aad,
            limits,
            random: SystemRandom::new(),
        };
        spool.clean_incomplete_temps()?;
        Ok(spool)
    }

    /// Durably publish an immutable encrypted frame before network delivery.
    ///
    /// Repeating the same sequence and plaintext is idempotent. Reusing a
    /// sequence for different plaintext fails without changing the durable
    /// frame.
    ///
    /// # Errors
    ///
    /// Returns a bounded [`SpoolError`] for an invalid immutable sequence,
    /// capacity exhaustion, corrupt existing data, or filesystem failure.
    pub fn put(&mut self, sequence: NonZeroU64, payload: &[u8]) -> Result<(), SpoolError> {
        self.validate_frame_length(payload.len(), false)?;
        if let Some((existing, _)) = self.read_entry(sequence)? {
            return if existing == payload {
                Ok(())
            } else {
                Err(SpoolError::InvalidInput(
                    "an output sequence cannot change after allocation",
                ))
            };
        }

        let body = self.encrypt_random(sequence, payload)?;
        let (frames, bytes) = self.usage()?;
        self.validate_new_capacity(frames, bytes, body.len())?;
        let temporary = self.write_temporary(&body)?;
        let final_name = frame_name(sequence);
        let link_result = linkat(
            &self.directory,
            temporary.as_str(),
            &self.directory,
            final_name.as_str(),
            AtFlags::empty(),
        );
        let result = match link_result {
            Ok(()) => sync_directory(&self.directory),
            Err(Errno::EXIST) => match self.read_entry(sequence)? {
                Some((existing, _)) if existing == payload => Ok(()),
                Some(_) => Err(SpoolError::InvalidInput(
                    "an output sequence cannot change after allocation",
                )),
                None => Err(SpoolError::InvalidInput(
                    "the output spool changed during publication",
                )),
            },
            Err(error) => Err(unavailable_errno(
                "the output spool frame cannot be published",
                error,
            )),
        };
        let cleanup = self.remove_temporary(&temporary);
        result.and(cleanup)
    }

    /// Restore every authenticated frame in numeric sequence order.
    ///
    /// # Errors
    ///
    /// Returns a bounded [`SpoolError`] for an unexpected directory entry,
    /// corrupt ciphertext, unsafe file, or filesystem failure.
    pub fn pending(&mut self) -> Result<Vec<SpooledFrame>, SpoolError> {
        let entries = self.entries(false)?;
        let mut frames = Vec::with_capacity(entries.len());
        for entry in entries {
            let sequence = entry.sequence.ok_or(SpoolError::InvalidInput(
                "the output spool contains an unexpected entry",
            ))?;
            let Some((payload, _)) = self.read_entry(sequence)? else {
                return Err(SpoolError::InvalidInput(
                    "the output spool changed during replay",
                ));
            };
            frames.push(SpooledFrame { sequence, payload });
        }
        Ok(frames)
    }

    /// Atomically compare and replace one exact durable plaintext frame.
    ///
    /// # Errors
    ///
    /// Returns a bounded [`SpoolError`] when the expected frame is absent or
    /// changed, the replacement exceeds capacity, data is corrupt, or the
    /// atomic filesystem operation fails.
    pub fn replace_exact(
        &mut self,
        sequence: NonZeroU64,
        expected: &[u8],
        replacement: &[u8],
    ) -> Result<(), SpoolError> {
        self.validate_frame_length(replacement.len(), true)?;
        let Some((current, current_size)) = self.read_entry(sequence)? else {
            return Err(SpoolError::InvalidInput(
                "the output spool changed before terminal replacement",
            ));
        };
        if current != expected {
            return Err(SpoolError::InvalidInput(
                "the output spool changed before terminal replacement",
            ));
        }

        let body = self.encrypt_random(sequence, replacement)?;
        let (frames, bytes) = self.usage()?;
        if frames > self.limits.max_frames {
            return Err(SpoolError::ResourceExhausted(
                "the encrypted output spool is full",
            ));
        }
        let replacement_bytes = u64::try_from(body.len())
            .map_err(|_| SpoolError::ResourceExhausted("the encrypted output spool is full"))?;
        let after_removal = bytes
            .checked_sub(current_size)
            .ok_or(SpoolError::InvalidInput(
                "the output spool usage is inconsistent",
            ))?;
        let total =
            after_removal
                .checked_add(replacement_bytes)
                .ok_or(SpoolError::ResourceExhausted(
                    "the encrypted output spool is full",
                ))?;
        if total > self.limits.max_encrypted_bytes {
            return Err(SpoolError::ResourceExhausted(
                "the encrypted output spool is full",
            ));
        }

        let temporary = self.write_temporary(&body)?;
        let final_name = frame_name(sequence);
        let result = renameat(
            &self.directory,
            temporary.as_str(),
            &self.directory,
            final_name.as_str(),
        )
        .map_err(|error| unavailable_errno("the output spool frame cannot be replaced", error))
        .and_then(|()| sync_directory(&self.directory));
        let cleanup = self.remove_temporary(&temporary);
        result.and(cleanup)
    }

    /// Set one exact pending frame aside, durably and still encrypted, so it is
    /// never replayed. The frame becomes a `.retired` record beside the live
    /// frames: it leaves `pending()` and the capacity accounting, and it blocks
    /// removal of the directory so the record outlives the execution's output.
    ///
    /// # Errors
    ///
    /// Returns a bounded [`SpoolError`] when the frame is absent or changed,
    /// too many frames are already retired, or the rename fails.
    pub fn retire_pending(
        &mut self,
        sequence: NonZeroU64,
        expected: &[u8],
    ) -> Result<(), SpoolError> {
        let Some((current, _)) = self.read_entry(sequence)? else {
            return Err(SpoolError::InvalidInput(
                "the output spool changed before retirement",
            ));
        };
        if current != expected {
            return Err(SpoolError::InvalidInput(
                "the output spool changed before retirement",
            ));
        }
        let retired = format!("{:020}.retired", sequence.get());
        renameat(
            &self.directory,
            frame_name(sequence).as_str(),
            &self.directory,
            retired.as_str(),
        )
        .map_err(|error| unavailable_errno("the output frame cannot be retired", error))?;
        sync_directory(&self.directory)?;
        // A bound check after the rename keeps the directory enumerable.
        self.entries(false).map(|_| ())
    }

    /// Whether any retired record remains in this spool.
    ///
    /// # Errors
    ///
    /// Returns a bounded [`SpoolError`] for unsafe entries or enumeration failure.
    pub fn has_retired_frames(&mut self) -> Result<bool, SpoolError> {
        let mut directory = Dir::read_from(&self.directory)
            .map_err(|error| unavailable_errno("the output spool cannot be enumerated", error))?;
        while let Some(entry) = directory.read() {
            let entry = entry.map_err(|error| {
                unavailable_errno("the output spool cannot be enumerated", error)
            })?;
            if classify_entry(entry.file_name()).is_ok_and(|kind| kind.retired) {
                return Ok(true);
            }
        }
        Ok(false)
    }

    /// Delete durably acknowledged frames through the inclusive sequence.
    ///
    /// # Errors
    ///
    /// Returns a bounded [`SpoolError`] for unsafe entries or filesystem
    /// failures. A partial unlink is safe because acknowledged replay is
    /// idempotent and the directory is synced only after the batch.
    pub fn acknowledge_through(&mut self, sequence: NonZeroU64) -> Result<(), SpoolError> {
        let entries = self.entries(false)?;
        let mut changed = false;
        for entry in entries {
            if entry.sequence.is_some_and(|value| value <= sequence) {
                unlinkat(&self.directory, entry.name.as_str(), AtFlags::empty()).map_err(
                    |error| unavailable_errno("the acknowledged output cannot be removed", error),
                )?;
                changed = true;
            }
        }
        if changed {
            sync_directory(&self.directory)?;
        }
        Ok(())
    }

    /// Remove the derived child directory only when it contains no entries.
    ///
    /// This is required by cutover preflight, which treats empty execution
    /// child directories as a nonempty replica spool root.
    ///
    /// # Errors
    ///
    /// Returns a bounded [`SpoolError`] for unexpected entries, identity
    /// replacement, removal, or durability failures.
    pub fn remove_if_empty(mut self) -> Result<bool, SpoolError> {
        if !self.entries(false)?.is_empty() || self.has_retired_frames()? {
            return Ok(false);
        }
        let held = fstat(&self.directory).map_err(|error| {
            unavailable_errno("the execution output spool is unavailable", error)
        })?;
        let named = statat(
            &self.root,
            self.child_name.as_str(),
            AtFlags::SYMLINK_NOFOLLOW,
        )
        .map_err(|error| unavailable_errno("the execution output spool is unavailable", error))?;
        if FileType::from_raw_mode(named.st_mode) != FileType::Directory
            || held.st_dev != named.st_dev
            || held.st_ino != named.st_ino
        {
            return Err(SpoolError::InvalidInput(
                "the execution output spool identity changed before removal",
            ));
        }
        let Self {
            root,
            directory,
            child_name,
            ..
        } = self;
        unlinkat(&root, child_name.as_str(), AtFlags::REMOVEDIR).map_err(|error| {
            unavailable_errno("the empty execution output spool cannot be removed", error)
        })?;
        sync_directory(&root)?;
        drop(directory);
        Ok(true)
    }

    fn validate_frame_length(&self, length: usize, replacement: bool) -> Result<(), SpoolError> {
        if length > self.limits.max_frame_bytes {
            return Err(SpoolError::ResourceExhausted(if replacement {
                "the replacement output frame exceeds the spool frame limit"
            } else {
                "the output frame exceeds the spool frame limit"
            }));
        }
        Ok(())
    }

    fn validate_new_capacity(
        &self,
        frames: usize,
        bytes: u64,
        body_length: usize,
    ) -> Result<(), SpoolError> {
        let next_frames = frames.checked_add(1).ok_or(SpoolError::ResourceExhausted(
            "the encrypted output spool is full",
        ))?;
        let body_length = u64::try_from(body_length)
            .map_err(|_| SpoolError::ResourceExhausted("the encrypted output spool is full"))?;
        let next_bytes = bytes
            .checked_add(body_length)
            .ok_or(SpoolError::ResourceExhausted(
                "the encrypted output spool is full",
            ))?;
        if next_frames > self.limits.max_frames || next_bytes > self.limits.max_encrypted_bytes {
            return Err(SpoolError::ResourceExhausted(
                "the encrypted output spool is full",
            ));
        }
        Ok(())
    }

    fn usage(&mut self) -> Result<(usize, u64), SpoolError> {
        let entries = self.entries(false)?;
        let mut bytes = 0_u64;
        for entry in &entries {
            bytes = bytes
                .checked_add(entry.size)
                .ok_or(SpoolError::ResourceExhausted(
                    "the encrypted output spool is full",
                ))?;
        }
        Ok((entries.len(), bytes))
    }

    #[allow(clippy::too_many_lines, reason = "One bounded pass validates every directory entry.")]
    fn entries(&mut self, allow_temporary: bool) -> Result<Vec<DirectoryEntry>, SpoolError> {
        let mut directory = Dir::read_from(&self.directory)
            .map_err(|error| unavailable_errno("the output spool cannot be enumerated", error))?;
        let mut entries = Vec::new();
        let mut final_frames = 0_usize;
        let mut retired_frames = 0_usize;
        let mut temporary_files = 0_usize;
        let mut final_bytes = 0_u64;
        let maximum_entry = u64::try_from(
            self.limits
                .max_frame_bytes
                .checked_add(FILE_OVERHEAD_BYTES)
                .ok_or(SpoolError::ResourceExhausted(
                    "the encrypted spool entry exceeds its approved limit",
                ))?,
        )
        .map_err(|_| {
            SpoolError::ResourceExhausted("the encrypted spool entry exceeds its approved limit")
        })?;
        while let Some(entry) = directory.read() {
            let entry = entry.map_err(|error| {
                unavailable_errno("the output spool cannot be enumerated", error)
            })?;
            let name = entry.file_name();
            if name.to_bytes() == b"." || name.to_bytes() == b".." {
                continue;
            }
            let kind = classify_entry(name)?;
            if kind.retired {
                // Set aside, never replayed: the record of a retired frame.
                let stat = statat(&self.directory, name, AtFlags::SYMLINK_NOFOLLOW).map_err(
                    |error| unavailable_errno("the output spool entry is unavailable", error),
                )?;
                validate_private_file_stat(&stat, false)?;
                retired_frames += 1;
                if retired_frames > MAX_RETIRED_FRAMES {
                    return Err(SpoolError::ResourceExhausted(
                        "the output spool holds too many retired frames",
                    ));
                }
                continue;
            }
            if kind.sequence.is_none() && !allow_temporary {
                return Err(SpoolError::InvalidInput(
                    "the output spool contains an unexpected entry",
                ));
            }
            let stat =
                statat(&self.directory, name, AtFlags::SYMLINK_NOFOLLOW).map_err(|error| {
                    unavailable_errno("the output spool entry is unavailable", error)
                })?;
            validate_private_file_stat(&stat, kind.sequence.is_none())?;
            let size = u64::try_from(stat.st_size).map_err(|_| {
                SpoolError::InvalidInput("the output spool entry size is malformed")
            })?;
            if size > maximum_entry {
                return Err(SpoolError::ResourceExhausted(
                    "the encrypted spool entry exceeds its approved limit",
                ));
            }
            if kind.sequence.is_some() {
                final_frames = final_frames
                    .checked_add(1)
                    .ok_or(SpoolError::ResourceExhausted(
                        "the encrypted output spool is full",
                    ))?;
                final_bytes =
                    final_bytes
                        .checked_add(size)
                        .ok_or(SpoolError::ResourceExhausted(
                            "the encrypted output spool is full",
                        ))?;
                if final_frames > self.limits.max_frames
                    || final_bytes > self.limits.max_encrypted_bytes
                {
                    return Err(SpoolError::ResourceExhausted(
                        "the encrypted output spool is full",
                    ));
                }
            } else {
                temporary_files =
                    temporary_files
                        .checked_add(1)
                        .ok_or(SpoolError::ResourceExhausted(
                            "the output spool contains excessive temporary residue",
                        ))?;
                if temporary_files > 1 {
                    return Err(SpoolError::ResourceExhausted(
                        "the output spool contains excessive temporary residue",
                    ));
                }
            }
            let name = name.to_str().map_err(|_| {
                SpoolError::InvalidInput("the output spool contains an unexpected entry")
            })?;
            entries.push(DirectoryEntry {
                name: name.to_owned(),
                sequence: kind.sequence,
                size,
            });
        }
        entries.sort_by_key(|entry| entry.sequence);
        Ok(entries)
    }

    fn read_entry(&self, sequence: NonZeroU64) -> Result<Option<(Vec<u8>, u64)>, SpoolError> {
        let name = frame_name(sequence);
        let file = match openat(
            &self.directory,
            name.as_str(),
            OFlags::RDONLY | OFlags::NONBLOCK | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::empty(),
        ) {
            Ok(file) => File::from(file),
            Err(Errno::NOENT) => return Ok(None),
            Err(error) => {
                return Err(unavailable_errno(
                    "the output spool entry is unavailable",
                    error,
                ));
            }
        };
        let stat = fstat(&file)
            .map_err(|error| unavailable_errno("the output spool entry is unavailable", error))?;
        validate_private_file_stat(&stat, false)?;
        let size = usize::try_from(stat.st_size)
            .map_err(|_| SpoolError::InvalidInput("the output spool entry size is malformed"))?;
        let maximum = self
            .limits
            .max_frame_bytes
            .checked_add(FILE_OVERHEAD_BYTES)
            .ok_or(SpoolError::ResourceExhausted(
                "the encrypted spool entry exceeds its approved limit",
            ))?;
        if size > maximum {
            return Err(SpoolError::ResourceExhausted(
                "the encrypted spool entry exceeds its approved limit",
            ));
        }
        if size < FILE_OVERHEAD_BYTES {
            return Err(SpoolError::InvalidInput(
                "the encrypted output spool is corrupt",
            ));
        }
        let mut body = vec![0; size];
        let mut file = file;
        file.read_exact(&mut body)
            .map_err(|error| unavailable("the output spool entry cannot be read", error))?;
        let mut extra = [0_u8; 1];
        if file
            .read(&mut extra)
            .map_err(|error| unavailable("the output spool entry cannot be read", error))?
            != 0
        {
            body.zeroize();
            return Err(SpoolError::InvalidInput(
                "the encrypted output spool changed while being read",
            ));
        }
        let plaintext = self.decrypt(sequence, body)?;
        Ok(Some((
            plaintext,
            u64::try_from(size).map_err(|_| {
                SpoolError::InvalidInput("the output spool entry size is malformed")
            })?,
        )))
    }

    fn encrypt_random(
        &self,
        sequence: NonZeroU64,
        plaintext: &[u8],
    ) -> Result<Vec<u8>, SpoolError> {
        let mut nonce = [0_u8; NONCE_BYTES];
        self.random.fill(&mut nonce).map_err(|_| {
            unavailable(
                "the output spool randomness is unavailable",
                io::Error::other("system randomness unavailable"),
            )
        })?;
        self.encrypt_with_nonce(sequence, plaintext, nonce)
    }

    fn encrypt_with_nonce(
        &self,
        sequence: NonZeroU64,
        plaintext: &[u8],
        nonce: [u8; NONCE_BYTES],
    ) -> Result<Vec<u8>, SpoolError> {
        let mut ciphertext = plaintext.to_vec();
        self.cipher
            .seal_in_place_append_tag(
                Nonce::assume_unique_for_key(nonce),
                Aad::from(self.entry_aad(sequence)),
                &mut ciphertext,
            )
            .map_err(|_| {
                unavailable(
                    "the output spool frame cannot be encrypted",
                    io::Error::other("authenticated encryption failed"),
                )
            })?;
        let mut body = Vec::with_capacity(FILE_MAGIC.len() + NONCE_BYTES + ciphertext.len());
        body.extend_from_slice(FILE_MAGIC);
        body.extend_from_slice(&nonce);
        body.extend_from_slice(&ciphertext);
        ciphertext.zeroize();
        Ok(body)
    }

    fn decrypt(&self, sequence: NonZeroU64, mut body: Vec<u8>) -> Result<Vec<u8>, SpoolError> {
        if !body.starts_with(FILE_MAGIC) || body.len() < FILE_OVERHEAD_BYTES {
            body.zeroize();
            return Err(SpoolError::InvalidInput(
                "the encrypted output spool is corrupt",
            ));
        }
        let nonce_start = FILE_MAGIC.len();
        let nonce_end = nonce_start + NONCE_BYTES;
        let nonce: [u8; NONCE_BYTES] = body[nonce_start..nonce_end]
            .try_into()
            .map_err(|_| SpoolError::InvalidInput("the encrypted output spool is corrupt"))?;
        let mut ciphertext = body.split_off(nonce_end);
        body.zeroize();
        let plaintext_length = if let Ok(plaintext) = self.cipher.open_in_place(
            Nonce::assume_unique_for_key(nonce),
            Aad::from(self.entry_aad(sequence)),
            &mut ciphertext,
        ) {
            plaintext.len()
        } else {
            ciphertext.zeroize();
            return Err(SpoolError::InvalidInput(
                "the encrypted output spool is corrupt",
            ));
        };
        ciphertext.truncate(plaintext_length);
        Ok(ciphertext)
    }

    fn entry_aad(&self, sequence: NonZeroU64) -> Vec<u8> {
        let mut aad = Vec::with_capacity(self.stream_aad.len() + 9);
        aad.extend_from_slice(&self.stream_aad);
        aad.push(0);
        aad.extend_from_slice(&sequence.get().to_be_bytes());
        aad
    }

    fn write_temporary(&self, body: &[u8]) -> Result<String, SpoolError> {
        for _ in 0..TEMP_CREATE_ATTEMPTS {
            let temporary = self.temporary_name()?;
            match openat(
                &self.directory,
                temporary.as_str(),
                OFlags::WRONLY | OFlags::CREATE | OFlags::EXCL | OFlags::NOFOLLOW | OFlags::CLOEXEC,
                Mode::RUSR | Mode::WUSR,
            ) {
                Ok(file) => {
                    let mut file = File::from(file);
                    let operation = validate_private_file(&file, true)
                        .and_then(|()| {
                            file.write_all(body).map_err(|error| {
                                unavailable("the output spool frame cannot be written", error)
                            })
                        })
                        .and_then(|()| {
                            file.sync_all().map_err(|error| {
                                unavailable("the output spool frame cannot be synced", error)
                            })
                        });
                    if let Err(error) = operation {
                        drop(file);
                        let cleanup = self.remove_temporary(&temporary);
                        return Err(cleanup.err().unwrap_or(error));
                    }
                    return Ok(temporary);
                }
                Err(Errno::EXIST) => {}
                Err(error) => {
                    return Err(unavailable_errno(
                        "the output spool temporary file is unavailable",
                        error,
                    ));
                }
            }
        }
        Err(SpoolError::Unavailable {
            message: "the output spool temporary name is unavailable",
            source: io::Error::new(io::ErrorKind::AlreadyExists, "temporary name collision"),
        })
    }

    fn temporary_name(&self) -> Result<String, SpoolError> {
        let mut random = [0_u8; 16];
        self.random.fill(&mut random).map_err(|_| {
            unavailable(
                "the output spool randomness is unavailable",
                io::Error::other("system randomness unavailable"),
            )
        })?;
        Ok(format!(".tmp-{}", lower_hex(&random)))
    }

    fn remove_temporary(&self, temporary: &str) -> Result<(), SpoolError> {
        match unlinkat(&self.directory, temporary, AtFlags::empty()) {
            Ok(()) => sync_directory(&self.directory),
            Err(Errno::NOENT) => Ok(()),
            Err(error) => Err(unavailable_errno(
                "the output spool temporary file cannot be removed",
                error,
            )),
        }
    }

    fn clean_incomplete_temps(&mut self) -> Result<(), SpoolError> {
        let entries = self.entries(true)?;
        let temporary = entries
            .into_iter()
            .filter(|entry| entry.sequence.is_none())
            .map(|entry| entry.name)
            .collect::<Vec<_>>();
        if temporary.is_empty() {
            return Ok(());
        }
        for name in temporary {
            unlinkat(&self.directory, name.as_str(), AtFlags::empty()).map_err(|error| {
                unavailable_errno("the output spool temporary file cannot be removed", error)
            })?;
        }
        sync_directory(&self.directory)
    }
}

struct DirectoryEntry {
    name: String,
    sequence: Option<NonZeroU64>,
    size: u64,
}

struct EntryKind {
    sequence: Option<NonZeroU64>,
    /// An encrypted frame set aside by [`EncryptedOutputSpool::retire_pending`].
    retired: bool,
}

fn encode_binding(components: &[&str]) -> Result<Vec<u8>, SpoolError> {
    let mut encoded = Vec::with_capacity(BINDING_DOMAIN.len() + components.len() * 32);
    encoded.extend_from_slice(BINDING_DOMAIN);
    for (index, component) in components.iter().enumerate() {
        if index != 0 {
            encoded.push(0);
        }
        let length = u32::try_from(component.len())
            .map_err(|_| SpoolError::InvalidInput("the execution spool identity is malformed"))?;
        encoded.extend_from_slice(&length.to_be_bytes());
        encoded.extend_from_slice(component.as_bytes());
    }
    Ok(encoded)
}

fn valid_identity(value: &str) -> bool {
    !value.is_empty() && value.len() <= MAX_IDENTITY_BYTES
}

fn validate_limits(limits: SpoolLimits) -> Result<(), SpoolError> {
    if limits.max_frames == 0 || limits.max_encrypted_bytes == 0 || limits.max_frame_bytes == 0 {
        return Err(SpoolError::InvalidInput(
            "the output spool limits are malformed",
        ));
    }
    if limits.max_frames > MAX_CONFIGURED_FRAMES
        || limits.max_encrypted_bytes > MAX_CONFIGURED_ENCRYPTED_BYTES
        || limits.max_frame_bytes > MAX_CONFIGURED_FRAME_BYTES
    {
        return Err(SpoolError::ResourceExhausted(
            "the output spool limits exceed protocol v1",
        ));
    }
    let maximum_entry = limits
        .max_frame_bytes
        .checked_add(FILE_OVERHEAD_BYTES)
        .and_then(|value| u64::try_from(value).ok())
        .ok_or(SpoolError::ResourceExhausted(
            "the output spool limits exceed the platform capacity",
        ))?;
    if maximum_entry > limits.max_encrypted_bytes {
        return Err(SpoolError::InvalidInput(
            "one encrypted output frame must fit in the spool",
        ));
    }
    Ok(())
}

fn derive_cipher(master_key: &SpoolMasterKey, binding: &[u8]) -> Result<LessSafeKey, SpoolError> {
    let mut key_bytes = derive_key_bytes(master_key, binding)?;
    let Ok(unbound) = UnboundKey::new(&aead::AES_256_GCM, &key_bytes) else {
        key_bytes.zeroize();
        return Err(unavailable(
            "the output spool key cannot be installed",
            io::Error::other("key installation failed"),
        ));
    };
    key_bytes.zeroize();
    Ok(LessSafeKey::new(unbound))
}

fn derive_key_bytes(master_key: &SpoolMasterKey, binding: &[u8]) -> Result<[u8; 32], SpoolError> {
    let salt = hkdf::Salt::new(hkdf::HKDF_SHA256, &[]);
    let prk = salt.extract(&master_key.0);
    let info = [KEY_DOMAIN, binding];
    let key = prk.expand(&info, &aead::AES_256_GCM).map_err(|_| {
        unavailable(
            "the output spool key cannot be derived",
            io::Error::other("key derivation failed"),
        )
    })?;
    let mut key_bytes = [0_u8; 32];
    if key.fill(&mut key_bytes).is_err() {
        key_bytes.zeroize();
        return Err(unavailable(
            "the output spool key cannot be derived",
            io::Error::other("key expansion failed"),
        ));
    }
    Ok(key_bytes)
}

fn classify_entry(name: &CStr) -> Result<EntryKind, SpoolError> {
    let name = name.to_bytes();
    if is_temporary_name(name) {
        return Ok(EntryKind {
            sequence: None,
            retired: false,
        });
    }
    let retired = name.len() == 28 && &name[20..] == b".retired";
    if !retired
        && (name.len() != 26 || !name[..20].iter().all(u8::is_ascii_digit) || &name[20..] != b".frame")
    {
        return Err(SpoolError::InvalidInput(
            "the output spool contains an unexpected entry",
        ));
    }
    let digits = std::str::from_utf8(&name[..20])
        .map_err(|_| SpoolError::InvalidInput("the output spool contains an unexpected entry"))?;
    let sequence =
        digits
            .parse::<u64>()
            .ok()
            .and_then(NonZeroU64::new)
            .ok_or(SpoolError::InvalidInput(
                "the output spool contains an unexpected entry",
            ))?;
    Ok(EntryKind {
        sequence: Some(sequence),
        retired,
    })
}

fn is_temporary_name(name: &[u8]) -> bool {
    name.len() == 37
        && name.starts_with(b".tmp-")
        && name[5..]
            .iter()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(byte))
}

fn frame_name(sequence: NonZeroU64) -> String {
    format!("{:020}.frame", sequence.get())
}

fn lower_hex(bytes: &[u8]) -> String {
    const DIGITS: &[u8; 16] = b"0123456789abcdef";
    let mut encoded = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        encoded.push(char::from(DIGITS[usize::from(byte >> 4)]));
        encoded.push(char::from(DIGITS[usize::from(byte & 0x0f)]));
    }
    encoded
}

fn validate_private_directory(file: &File, message: &'static str) -> Result<(), SpoolError> {
    let stat = fstat(file).map_err(|error| unavailable_errno(message, error))?;
    if FileType::from_raw_mode(stat.st_mode) != FileType::Directory
        || stat.st_uid != geteuid().as_raw()
        || stat.st_mode & 0o077 != 0
    {
        return Err(SpoolError::InvalidInput(message));
    }
    Ok(())
}

fn validate_private_file(file: &File, temporary: bool) -> Result<(), SpoolError> {
    let stat = fstat(file)
        .map_err(|error| unavailable_errno("the output spool entry is unavailable", error))?;
    validate_private_file_stat(&stat, temporary)
}

fn validate_private_file_stat(stat: &rustix::fs::Stat, temporary: bool) -> Result<(), SpoolError> {
    if FileType::from_raw_mode(stat.st_mode) != FileType::RegularFile
        || stat.st_uid != geteuid().as_raw()
        || stat.st_mode & 0o077 != 0
    {
        return Err(SpoolError::InvalidInput(if temporary {
            "the output spool contains an unsafe temporary entry"
        } else {
            "the output spool contains an unsafe frame entry"
        }));
    }
    Ok(())
}

fn sync_directory(directory: &File) -> Result<(), SpoolError> {
    fsync(directory)
        .map_err(|error| unavailable_errno("the output spool directory cannot be synced", error))
}

fn unavailable(message: &'static str, source: io::Error) -> SpoolError {
    SpoolError::Unavailable { message, source }
}

fn unavailable_errno(message: &'static str, source: Errno) -> SpoolError {
    unavailable(message, source.into())
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;
    use std::fs::File;

    use super::{
        AAD_DOMAIN, EncryptedOutputSpool, ExecutionSpoolBinding, ExecutionSpoolIdentity,
        SpoolLimits, SpoolMasterKey, derive_cipher, derive_key_bytes, lower_hex,
    };
    use ring::aead::LessSafeKey;
    use zeroize::Zeroize;

    fn binding() -> ExecutionSpoolBinding {
        ExecutionSpoolBinding::new(&ExecutionSpoolIdentity {
            tenant_id: "tenant-1".to_owned(),
            resource_project_id: "resource-2".to_owned(),
            projection_project_id: "projection-3".to_owned(),
            command_id: "command-4".to_owned(),
            execution_id: "execution-5".to_owned(),
            generation: 7,
            producer_id: "producer-6".to_owned(),
        })
        .expect("valid fixture binding")
    }

    #[test]
    fn binding_hkdf_and_fixed_nonce_ciphertext_match_python() {
        let vectors = include_str!("../tests/fixtures/output_spool_vectors.txt")
            .lines()
            .map(|line| line.split_once('=').expect("named Python spool vector"))
            .collect::<BTreeMap<_, _>>();
        let binding = binding();
        assert_eq!(lower_hex(&binding.encoded), vectors["binding"]);
        assert_eq!(binding.directory_name(), vectors["directory"]);
        let master = SpoolMasterKey::new(std::array::from_fn(|index| {
            u8::try_from(index).expect("32-byte key index")
        }));
        let mut key_bytes = derive_key_bytes(&master, &binding.encoded).expect("derived key bytes");
        assert_eq!(lower_hex(&key_bytes), vectors["derived_key"]);
        key_bytes.zeroize();
        let cipher: LessSafeKey = derive_cipher(&master, &binding.encoded).expect("derived key");
        let mut stream_aad = Vec::from(AAD_DOMAIN);
        stream_aad.extend_from_slice(&binding.encoded);
        let spool = EncryptedOutputSpool {
            root: File::open(".").expect("current directory"),
            directory: File::open(".").expect("current directory"),
            child_name: String::new(),
            cipher,
            stream_aad,
            limits: SpoolLimits {
                max_frames: 1,
                max_encrypted_bytes: 4096,
                max_frame_bytes: 1024,
            },
            random: ring::rand::SystemRandom::new(),
        };
        let body = spool
            .encrypt_with_nonce(
                std::num::NonZeroU64::new(1).expect("nonzero"),
                b"sensitive-output",
                std::array::from_fn(|index| u8::try_from(index).expect("12-byte nonce index")),
            )
            .expect("fixed nonce encryption");
        assert_eq!(lower_hex(&body), vectors["fixed_nonce_body"]);
    }
}
