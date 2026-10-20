//! The Qwen3 decoder as an embedder: one causal pass over right-padded
//! sequences, no key/value cache, attention through candle's fused `sdpa`
//! kernel (causal, grouped-query) on Metal.
//!
//! candle-transformers' `qwen3::Model` builds its causal mask with one
//! batch row's worth of values on the GPU path, so every sequence but one
//! of a batch came back wrong on Metal (cosine ~0.1 with the same text
//! embedded alone; the CPU path was right). This is the same network, read
//! from the same weights, without the cache an embedder never uses.

use candle_core::{DType, Device, Module as _, Result, Tensor};
use candle_nn::{Embedding, Linear, RmsNorm, VarBuilder, embedding, linear_no_bias, rms_norm};
use serde::Deserialize;

/// What the checkpoint's `config.json` says (the fields this reads).
#[derive(Debug, Clone, Deserialize)]
pub struct Config {
    pub vocab_size: usize,
    pub hidden_size: usize,
    pub intermediate_size: usize,
    pub num_hidden_layers: usize,
    pub num_attention_heads: usize,
    pub num_key_value_heads: usize,
    pub head_dim: usize,
    pub max_position_embeddings: usize,
    pub rope_theta: f64,
    pub rms_norm_eps: f64,
}

struct Layer {
    input_norm: RmsNorm,
    q: Linear,
    k: Linear,
    v: Linear,
    o: Linear,
    q_norm: RmsNorm,
    k_norm: RmsNorm,
    post_norm: RmsNorm,
    gate: Linear,
    up: Linear,
    down: Linear,
}

/// The decoder (without a language-model head).
pub struct Qwen3 {
    embed: Embedding,
    layers: Vec<Layer>,
    norm: RmsNorm,
    heads: usize,
    kv_heads: usize,
    head_dim: usize,
    cos: Tensor,
    sin: Tensor,
}

fn rotary(
    config: &Config,
    longest: usize,
    dtype: DType,
    device: &Device,
) -> Result<(Tensor, Tensor)> {
    let dim = config.head_dim;
    #[allow(clippy::cast_possible_truncation, clippy::cast_precision_loss)]
    let inverse: Vec<f32> = (0..dim)
        .step_by(2)
        .map(|i| 1.0 / config.rope_theta.powf(i as f64 / dim as f64) as f32)
        .collect();
    let half = inverse.len();
    let inverse = Tensor::from_vec(inverse, (1, half), device)?;
    let positions = Tensor::arange(0_u32, u32::try_from(longest).unwrap_or(u32::MAX), device)?
        .to_dtype(DType::F32)?
        .reshape((longest, 1))?;
    let freqs = positions.matmul(&inverse)?;
    Ok((freqs.cos()?.to_dtype(dtype)?, freqs.sin()?.to_dtype(dtype)?))
}

impl Qwen3 {
    /// Load the decoder from `vb` (names as Qwen3-Embedding saves them:
    /// `embed_tokens`, `layers.N.*`, `norm`). Positions are prepared up to
    /// `longest` tokens.
    ///
    /// # Errors
    ///
    /// A tensor is missing or of another shape.
    pub fn load(vb: &VarBuilder<'_>, config: &Config, longest: usize) -> Result<Self> {
        let hidden = config.hidden_size;
        let eps = config.rms_norm_eps;
        let hd = config.head_dim;
        let mut layers = Vec::with_capacity(config.num_hidden_layers);
        for i in 0..config.num_hidden_layers {
            let l = vb.pp(format!("layers.{i}"));
            let a = l.pp("self_attn");
            let m = l.pp("mlp");
            layers.push(Layer {
                input_norm: rms_norm(hidden, eps, l.pp("input_layernorm"))?,
                q: linear_no_bias(hidden, config.num_attention_heads * hd, a.pp("q_proj"))?,
                k: linear_no_bias(hidden, config.num_key_value_heads * hd, a.pp("k_proj"))?,
                v: linear_no_bias(hidden, config.num_key_value_heads * hd, a.pp("v_proj"))?,
                o: linear_no_bias(config.num_attention_heads * hd, hidden, a.pp("o_proj"))?,
                q_norm: rms_norm(hd, eps, a.pp("q_norm"))?,
                k_norm: rms_norm(hd, eps, a.pp("k_norm"))?,
                post_norm: rms_norm(hidden, eps, l.pp("post_attention_layernorm"))?,
                gate: linear_no_bias(hidden, config.intermediate_size, m.pp("gate_proj"))?,
                up: linear_no_bias(hidden, config.intermediate_size, m.pp("up_proj"))?,
                down: linear_no_bias(config.intermediate_size, hidden, m.pp("down_proj"))?,
            });
        }
        let longest = longest.min(config.max_position_embeddings);
        let (cos, sin) = rotary(config, longest, vb.dtype(), vb.device())?;
        Ok(Self {
            embed: embedding(config.vocab_size, hidden, vb.pp("embed_tokens"))?,
            layers,
            norm: rms_norm(hidden, eps, vb.pp("norm"))?,
            heads: config.num_attention_heads,
            kv_heads: config.num_key_value_heads,
            head_dim: hd,
            cos,
            sin,
        })
    }

    /// The last hidden states `(batch, len, hidden)` of right-padded `ids`
    /// `(batch, len)`.
    ///
    /// # Errors
    ///
    /// A kernel failed, or `len` is longer than the positions prepared.
    #[allow(clippy::many_single_char_names)] // q, k, v, x, h: the paper's names
    pub fn forward(&self, ids: &Tensor) -> Result<Tensor> {
        let (batch, len) = ids.dims2()?;
        let device = ids.device();
        let cos = self.cos.narrow(0, 0, len)?;
        let sin = self.sin.narrow(0, 0, len)?;
        let fused = !device.is_cpu();
        let mut x = self.embed.forward(ids)?;
        #[allow(clippy::cast_precision_loss)]
        let scale = 1.0 / (self.head_dim as f64).sqrt();
        let causal = if fused || len < 2 {
            None
        } else {
            let values: Vec<f32> = (0..len)
                .flat_map(|i| (0..len).map(move |j| if j <= i { 0.0 } else { f32::NEG_INFINITY }))
                .collect();
            Some(Tensor::from_vec(values, (1, 1, len, len), device)?.to_dtype(x.dtype())?)
        };
        for layer in &self.layers {
            let h = layer.input_norm.forward(&x)?;
            let split = |t: Tensor, heads: usize, norm: Option<&RmsNorm>| -> Result<Tensor> {
                let t = t.reshape((batch, len, heads, self.head_dim))?;
                let t = match norm {
                    Some(norm) => norm.forward(&t)?,
                    None => t,
                };
                t.transpose(1, 2)?.contiguous()
            };
            let q = split(layer.q.forward(&h)?, self.heads, Some(&layer.q_norm))?;
            let k = split(layer.k.forward(&h)?, self.kv_heads, Some(&layer.k_norm))?;
            let v = split(layer.v.forward(&h)?, self.kv_heads, None)?;
            let q = candle_nn::rotary_emb::rope(&q, &cos, &sin)?;
            let k = candle_nn::rotary_emb::rope(&k, &cos, &sin)?;
            let context = if fused {
                #[allow(clippy::cast_possible_truncation)]
                candle_nn::ops::sdpa(&q, &k, &v, None, len > 1, scale as f32, 1.0)?
            } else {
                let groups = self.heads / self.kv_heads;
                let repeat = |t: Tensor| -> Result<Tensor> {
                    if groups == 1 {
                        return Ok(t);
                    }
                    let (b, h, l, d) = t.dims4()?;
                    t.unsqueeze(2)?
                        .broadcast_as((b, h, groups, l, d))?
                        .reshape((b, h * groups, l, d))
                };
                let k = repeat(k)?.contiguous()?;
                let v = repeat(v)?.contiguous()?;
                let mut scores = (q.matmul(&k.t()?)? * scale)?;
                if let Some(mask) = &causal {
                    scores = scores.broadcast_add(mask)?;
                }
                candle_nn::ops::softmax_last_dim(&scores)?.matmul(&v)?
            };
            let context =
                context
                    .transpose(1, 2)?
                    .reshape((batch, len, self.heads * self.head_dim))?;
            x = (x + layer.o.forward(&context)?)?;
            let h = layer.post_norm.forward(&x)?;
            let mlp = layer.down.forward(
                &(candle_nn::ops::silu(&layer.gate.forward(&h)?)? * layer.up.forward(&h)?)?,
            )?;
            x = (x + mlp)?;
        }
        self.norm.forward(&x)
    }
}
