//! The catalogue's models on this computer, with candle: the GPU of Apple
//! silicon (on macOS) when there is one, the CPU otherwise.
//!
//! * multilingual-e5 (BERT): `passage: ` / `query: ` prefixes, mean pooling
//!   over the attention mask;
//! * Qwen3-Embedding (decoder): the query instruction, last-token pooling.
//!   Sequences are padded on the RIGHT: the model is causal, so a real
//!   token never attends to the padding after it, and the vector is read at
//!   each sequence's own last token.
//!
//! A call sorts its texts by length and runs them in micro-batches of
//! similar lengths (little padding), then puts the vectors back in order.

use crate::catalog::{Family, ModelSpec};
use crate::vector::{normalize, truncate};
use crate::{EmbedError, Embedder};
use crate::{bert, qwen3};
use candle_core::{DType, Device, IndexOp as _, Tensor};
use candle_nn::VarBuilder;
use std::path::Path;
use tokenizers::{Tokenizer, TruncationParams};

/// Where the model runs.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DevicePreference {
    /// The GPU when the build and the computer have one, else the CPU.
    Auto,
    Cpu,
}

enum Network {
    Bert(bert::Bert, DType),
    Qwen3(qwen3::Qwen3),
}

/// One loaded model.
pub struct CandleEmbedder {
    spec: &'static ModelSpec,
    tokenizer: Tokenizer,
    network: Network,
    device: Device,
    micro_batch: usize,
}

impl std::fmt::Debug for CandleEmbedder {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CandleEmbedder")
            .field("model", &self.spec.id)
            .field("device", &self.device_name())
            .finish_non_exhaustive()
    }
}

fn load_error(error: impl std::fmt::Display) -> EmbedError {
    EmbedError::Load(error.to_string())
}

fn run_error(error: impl std::fmt::Display) -> EmbedError {
    EmbedError::Run(error.to_string())
}

fn pick_device(preference: DevicePreference) -> Device {
    match preference {
        DevicePreference::Cpu => Device::Cpu,
        DevicePreference::Auto => {
            #[cfg(target_os = "macos")]
            if let Ok(device) = Device::new_metal(0) {
                return device;
            }
            Device::Cpu
        }
    }
}

/// Texts per forward pass: what ran fastest on an M4 Pro (S0: e5-small
/// 62 chunks/s at 8, 58 at 16, 53 at 32; Qwen3 11.8 at 4, 9.7 at 16).
fn micro_batch(family: Family, gpu: bool) -> usize {
    match (family, gpu) {
        (Family::Qwen3, true) => 4,
        _ => 8,
    }
}

impl CandleEmbedder {
    /// Load `spec` from `dir` (its verified files). Blocking; takes seconds.
    ///
    /// # Errors
    ///
    /// A file is missing or does not load.
    pub fn load(
        spec: &'static ModelSpec,
        dir: &Path,
        preference: DevicePreference,
    ) -> Result<Self, EmbedError> {
        let device = pick_device(preference);
        let mut tokenizer = Tokenizer::from_file(dir.join("tokenizer.json")).map_err(load_error)?;
        tokenizer
            .with_truncation(Some(TruncationParams {
                max_length: spec.max_tokens,
                ..TruncationParams::default()
            }))
            .map_err(load_error)?;
        tokenizer.with_padding(None);
        let config = std::fs::read_to_string(dir.join("config.json")).map_err(load_error)?;
        let weights = [dir.join("model.safetensors")];
        let gpu = !device.is_cpu();
        let network = match spec.family {
            Family::Bert => {
                let config: bert::Config = serde_json::from_str(&config).map_err(load_error)?;
                // F32: half precision bought no speed for the small encoder (S0).
                let dtype = DType::F32;
                let vb = load_weights(&weights, dtype, &device)?;
                Network::Bert(bert::Bert::load(&vb, &config).map_err(load_error)?, dtype)
            }
            Family::Qwen3 => {
                let config: qwen3::Config = serde_json::from_str(&config).map_err(load_error)?;
                let dtype = if gpu { DType::F16 } else { DType::F32 };
                let vb = load_weights(&weights, dtype, &device)?;
                Network::Qwen3(
                    qwen3::Qwen3::load(&vb, &config, spec.max_tokens).map_err(load_error)?,
                )
            }
        };
        Ok(Self {
            spec,
            tokenizer,
            network,
            device,
            micro_batch: micro_batch(spec.family, gpu),
        })
    }

    /// `metal` or `cpu`.
    #[must_use]
    pub fn device_name(&self) -> &'static str {
        if self.device.is_cpu() { "cpu" } else { "metal" }
    }

    /// The model it runs.
    #[must_use]
    pub fn spec(&self) -> &'static ModelSpec {
        self.spec
    }

    fn embed_prepared(&self, texts: Vec<String>) -> Result<Vec<Vec<f32>>, EmbedError> {
        let encodings = self
            .tokenizer
            .encode_batch(texts, true)
            .map_err(run_error)?;
        let mut order: Vec<usize> = (0..encodings.len()).collect();
        order.sort_by_key(|&i| encodings[i].get_ids().len());
        let mut out: Vec<Option<Vec<f32>>> = vec![None; encodings.len()];
        for group in order.chunks(self.micro_batch.max(1)) {
            let ids: Vec<&[u32]> = group.iter().map(|&i| encodings[i].get_ids()).collect();
            let vectors = self.forward(&ids)?;
            for (&i, vector) in group.iter().zip(vectors) {
                out[i] = Some(vector);
            }
        }
        out.into_iter()
            .map(|vector| vector.ok_or_else(|| EmbedError::Shape("a vector is missing".into())))
            .collect()
    }

    /// One micro-batch, right-padded: its pooled, normalised vectors.
    fn forward(&self, ids: &[&[u32]]) -> Result<Vec<Vec<f32>>, EmbedError> {
        let width = ids.iter().map(|ids| ids.len()).max().unwrap_or(0).max(1);
        let rows = ids.len();
        let mut flat = Vec::with_capacity(rows * width);
        let mut mask = Vec::with_capacity(rows * width);
        for ids in ids {
            flat.extend_from_slice(ids);
            mask.extend(std::iter::repeat_n(1_u32, ids.len()));
            flat.extend(std::iter::repeat_n(0_u32, width - ids.len()));
            mask.extend(std::iter::repeat_n(0_u32, width - ids.len()));
        }
        let input = Tensor::from_vec(flat, (rows, width), &self.device).map_err(run_error)?;
        let mask = Tensor::from_vec(mask, (rows, width), &self.device).map_err(run_error)?;
        let pooled = match &self.network {
            Network::Bert(model, dtype) => {
                let hidden = model.forward(&input, &mask, *dtype).map_err(run_error)?;
                // Mean over the real tokens.
                let weights = mask
                    .to_dtype(hidden.dtype())
                    .and_then(|m| m.unsqueeze(2))
                    .map_err(run_error)?;
                let summed = hidden
                    .broadcast_mul(&weights)
                    .and_then(|h| h.sum(1))
                    .map_err(run_error)?;
                let counts = weights.sum(1).map_err(run_error)?;
                summed.broadcast_div(&counts).map_err(run_error)?
            }
            Network::Qwen3(model) => {
                let hidden = model.forward(&input).map_err(run_error)?;
                let mut last = Vec::with_capacity(rows);
                for (row, ids) in ids.iter().enumerate() {
                    let at = ids.len().saturating_sub(1);
                    last.push(hidden.i((row, at)).map_err(run_error)?);
                }
                Tensor::stack(&last, 0).map_err(run_error)?
            }
        };
        let pooled = pooled
            .to_dtype(DType::F32)
            .and_then(|t| t.to_vec2::<f32>())
            .map_err(run_error)?;
        Ok(pooled
            .into_iter()
            .map(|mut vector| {
                normalize(&mut vector);
                truncate(vector, self.spec.dimension)
            })
            .collect())
    }
}

/// The weights, read whole (the workspace forbids `unsafe`, so they are
/// not memory-mapped).
fn load_weights(
    weights: &[std::path::PathBuf],
    dtype: DType,
    device: &Device,
) -> Result<VarBuilder<'static>, EmbedError> {
    let tensors = candle_core::safetensors::load(&weights[0], device).map_err(load_error)?;
    Ok(VarBuilder::from_tensors(tensors, dtype, device))
}

impl Embedder for CandleEmbedder {
    fn stamp(&self) -> String {
        self.spec.stamp()
    }

    fn dimension(&self) -> usize {
        self.spec.dimension
    }

    fn embed_documents(&self, texts: &[&str]) -> Result<Vec<Vec<f32>>, EmbedError> {
        if texts.is_empty() {
            return Ok(Vec::new());
        }
        let prepared = texts.iter().map(|t| self.spec.document_text(t)).collect();
        self.embed_prepared(prepared)
    }

    fn embed_query(&self, text: &str) -> Result<Vec<f32>, EmbedError> {
        self.embed_prepared(vec![self.spec.query_text(text)])?
            .pop()
            .ok_or_else(|| EmbedError::Shape("no vector".into()))
    }
}
