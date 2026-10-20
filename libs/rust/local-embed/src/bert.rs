//! A BERT encoder (the multilingual-e5 checkpoints) whose attention is
//! candle's fused `sdpa` kernel on Metal, with the padding as a broadcast
//! additive mask.
//!
//! candle-transformers' own `BertModel` materialises every attention matrix
//! and runs softmax as separate kernels; on an M4 Pro that made
//! multilingual-e5-small embed ~11 chunks/s at 512 tokens. The fused kernel
//! is the difference (see the crate's `embed_bench`). The CPU path keeps the
//! plain matmul/softmax attention.
//!
//! Weights are read under the checkpoint's own names (`embeddings.*`,
//! `encoder.layer.N.*`, as `BertModel.save_pretrained` writes them).

use candle_core::{DType, Module as _, Result, Tensor};
use candle_nn::{Embedding, LayerNorm, Linear, VarBuilder, embedding, layer_norm, linear};
use serde::Deserialize;

/// What the checkpoint's `config.json` says (the fields this reads).
#[derive(Debug, Clone, Deserialize)]
pub struct Config {
    pub vocab_size: usize,
    pub hidden_size: usize,
    pub num_hidden_layers: usize,
    pub num_attention_heads: usize,
    pub intermediate_size: usize,
    pub max_position_embeddings: usize,
    pub type_vocab_size: usize,
    pub layer_norm_eps: f64,
}

struct Layer {
    query: Linear,
    key: Linear,
    value: Linear,
    attention_out: Linear,
    attention_norm: LayerNorm,
    intermediate: Linear,
    output: Linear,
    output_norm: LayerNorm,
}

/// The encoder.
pub struct Bert {
    words: Embedding,
    positions: Embedding,
    types: Embedding,
    embeddings_norm: LayerNorm,
    layers: Vec<Layer>,
    heads: usize,
    head_dim: usize,
}

impl Bert {
    /// Load the encoder from `vb`.
    ///
    /// # Errors
    ///
    /// A tensor is missing or of another shape.
    pub fn load(vb: &VarBuilder<'_>, config: &Config) -> Result<Self> {
        let hidden = config.hidden_size;
        let eps = config.layer_norm_eps;
        let e = vb.pp("embeddings");
        let mut layers = Vec::with_capacity(config.num_hidden_layers);
        for i in 0..config.num_hidden_layers {
            let l = vb.pp(format!("encoder.layer.{i}"));
            layers.push(Layer {
                query: linear(hidden, hidden, l.pp("attention.self.query"))?,
                key: linear(hidden, hidden, l.pp("attention.self.key"))?,
                value: linear(hidden, hidden, l.pp("attention.self.value"))?,
                attention_out: linear(hidden, hidden, l.pp("attention.output.dense"))?,
                attention_norm: layer_norm(hidden, eps, l.pp("attention.output.LayerNorm"))?,
                intermediate: linear(hidden, config.intermediate_size, l.pp("intermediate.dense"))?,
                output: linear(config.intermediate_size, hidden, l.pp("output.dense"))?,
                output_norm: layer_norm(hidden, eps, l.pp("output.LayerNorm"))?,
            });
        }
        Ok(Self {
            words: embedding(config.vocab_size, hidden, e.pp("word_embeddings"))?,
            positions: embedding(
                config.max_position_embeddings,
                hidden,
                e.pp("position_embeddings"),
            )?,
            types: embedding(
                config.type_vocab_size,
                hidden,
                e.pp("token_type_embeddings"),
            )?,
            embeddings_norm: layer_norm(hidden, eps, e.pp("LayerNorm"))?,
            layers,
            heads: config.num_attention_heads,
            head_dim: hidden / config.num_attention_heads,
        })
    }

    /// The last hidden states `(batch, len, hidden)` of right-padded `ids`
    /// with `mask` (1 = token, 0 = padding), both `(batch, len)` u32.
    ///
    /// # Errors
    ///
    /// A kernel failed.
    #[allow(clippy::many_single_char_names)] // q, k, v, x: the paper's names
    pub fn forward(&self, ids: &Tensor, mask: &Tensor, dtype: DType) -> Result<Tensor> {
        let (batch, len) = ids.dims2()?;
        let device = ids.device();
        let positions = Tensor::arange(0_u32, u32::try_from(len).unwrap_or(u32::MAX), device)?
            .unsqueeze(0)?
            .broadcast_as((batch, len))?
            .contiguous()?;
        let types = ids.zeros_like()?;
        let mut x = self
            .words
            .forward(ids)?
            .add(&self.positions.forward(&positions)?)?
            .add(&self.types.forward(&types)?)?;
        x = self.embeddings_norm.forward(&x)?;
        // Additive: 0 for a token, a large negative number for padding,
        // broadcast over heads and query positions (strides 0, no copy).
        let negative = if dtype == DType::F16 { -6.0e4 } else { -1.0e9 };
        let additive = ((mask.to_dtype(DType::F32)? - 1.0)? * (-negative))?.to_dtype(dtype)?;
        let additive = additive
            .reshape((batch, 1, 1, len))?
            .broadcast_as((batch, self.heads, len, len))?;
        #[allow(clippy::cast_precision_loss)]
        let scale = 1.0 / (self.head_dim as f64).sqrt();
        let fused = !device.is_cpu();
        for layer in &self.layers {
            let heads = |t: Tensor| -> Result<Tensor> {
                t.reshape((batch, len, self.heads, self.head_dim))?
                    .transpose(1, 2)?
                    .contiguous()
            };
            let q = heads(layer.query.forward(&x)?)?;
            let k = heads(layer.key.forward(&x)?)?;
            let v = heads(layer.value.forward(&x)?)?;
            let context = if fused {
                #[allow(clippy::cast_possible_truncation)]
                candle_nn::ops::sdpa(&q, &k, &v, Some(&additive), false, scale as f32, 1.0)?
            } else {
                let scores = (q.matmul(&k.t()?)? * scale)?.broadcast_add(&additive)?;
                candle_nn::ops::softmax_last_dim(&scores)?.matmul(&v)?
            };
            let context =
                context
                    .transpose(1, 2)?
                    .reshape((batch, len, self.heads * self.head_dim))?;
            let attended = layer
                .attention_norm
                .forward(&layer.attention_out.forward(&context)?.add(&x)?)?;
            let inner = layer.intermediate.forward(&attended)?.gelu_erf()?;
            x = layer
                .output_norm
                .forward(&layer.output.forward(&inner)?.add(&attended)?)?;
        }
        Ok(x)
    }
}
