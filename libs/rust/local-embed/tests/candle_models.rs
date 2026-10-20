//! The on-device backend against the real checkpoints. Skipped unless
//! `ELITEA_EMBED_MODELS` names a folder with `e5/` and/or `qwen3/` holding
//! the catalogue's files (they are 0.5-1.2 GB; CI does not download them).
//!
//! * the fused-attention BERT gives the vectors candle-transformers'
//!   reference `BertModel` gives (cosine > 0.999), padded or not;
//! * a query is closer to the passage that answers it than to others.
#![cfg(feature = "candle")]

use candle_core::{DType, Device, Tensor};
use candle_transformers::models::bert as reference;
use elitea_local_embed::Embedder as _;
use elitea_local_embed::candle_backend::{CandleEmbedder, DevicePreference};
use elitea_local_embed::catalog::{MULTILINGUAL_E5_SMALL, QWEN3_EMBEDDING_0_6B};
use std::path::PathBuf;

fn models() -> Option<PathBuf> {
    std::env::var_os("ELITEA_EMBED_MODELS").map(PathBuf::from)
}

fn cosine(a: &[f32], b: &[f32]) -> f32 {
    a.iter().zip(b).map(|(x, y)| x * y).sum()
}

const PASSAGES: [&str; 3] = [
    "fn read_config(path: &Path) -> io::Result<Config> { let text = fs::read_to_string(path)?; toml::from_str(&text) }",
    "The scheduler pauses background embedding while a turn runs and resumes after it ends.",
    "Bananas are a good source of potassium and are eaten all over the world.",
];

fn reference_e5(dir: &std::path::Path, texts: &[String]) -> Vec<Vec<f32>> {
    let device = Device::Cpu;
    let config: reference::Config =
        serde_json::from_str(&std::fs::read_to_string(dir.join("config.json")).unwrap()).unwrap();
    let tensors = candle_core::safetensors::load(dir.join("model.safetensors"), &device).unwrap();
    let vb = candle_nn::VarBuilder::from_tensors(tensors, DType::F32, &device);
    let model = reference::BertModel::load(vb, &config).unwrap();
    let tokenizer = tokenizers::Tokenizer::from_file(dir.join("tokenizer.json")).unwrap();
    texts
        .iter()
        .map(|text| {
            let ids = tokenizer
                .encode(text.as_str(), true)
                .unwrap()
                .get_ids()
                .to_vec();
            let n = ids.len();
            let ids = Tensor::from_vec(ids, (1, n), &device).unwrap();
            let types = ids.zeros_like().unwrap();
            let hidden = model.forward(&ids, &types, None).unwrap();
            let mut v: Vec<f32> = (hidden.sum(1).unwrap() / f64::from(u32::try_from(n).unwrap()))
                .unwrap()
                .squeeze(0)
                .unwrap()
                .to_vec1()
                .unwrap();
            elitea_local_embed::vector::normalize(&mut v);
            v
        })
        .collect()
}

#[test]
fn e5_matches_the_reference_encoder_and_ranks_the_answer_first() {
    let Some(root) = models() else { return };
    let dir = root.join("e5");
    if !dir.is_dir() {
        return;
    }
    let prepared: Vec<String> = PASSAGES
        .iter()
        .map(|p| MULTILINGUAL_E5_SMALL.document_text(p))
        .collect();
    let expected = reference_e5(&dir, &prepared);
    for preference in [DevicePreference::Cpu, DevicePreference::Auto] {
        let embedder = CandleEmbedder::load(&MULTILINGUAL_E5_SMALL, &dir, preference).unwrap();
        // One batch of different lengths: padding must not change a vector.
        let got = embedder.embed_documents(&PASSAGES).unwrap();
        for (got, want) in got.iter().zip(&expected) {
            let c = cosine(got, want);
            assert!(c > 0.999, "{}: cosine {c}", embedder.device_name());
        }
        let query = embedder.embed_query("how is a config file read").unwrap();
        let scores: Vec<f32> = got.iter().map(|p| cosine(&query, p)).collect();
        assert!(scores[0] > scores[1] && scores[0] > scores[2], "{scores:?}");
    }
}

/// candle-transformers' Qwen3 on the CPU, one text at a time (its CPU
/// path is right; its GPU path is what `qwen3.rs` replaces).
fn reference_qwen3(dir: &std::path::Path, texts: &[&str]) -> Vec<Vec<f32>> {
    use candle_transformers::models::qwen3 as reference;
    let device = Device::Cpu;
    let config: reference::Config =
        serde_json::from_str(&std::fs::read_to_string(dir.join("config.json")).unwrap()).unwrap();
    let tensors = candle_core::safetensors::load(dir.join("model.safetensors"), &device).unwrap();
    let vb = candle_nn::VarBuilder::from_tensors(tensors, DType::F32, &device)
        .rename_f(|name: &str| name.strip_prefix("model.").unwrap_or(name).to_owned());
    let model = reference::Model::new(&config, vb).unwrap();
    let tokenizer = tokenizers::Tokenizer::from_file(dir.join("tokenizer.json")).unwrap();
    texts
        .iter()
        .map(|text| {
            let ids = tokenizer.encode(*text, true).unwrap().get_ids().to_vec();
            let n = ids.len();
            let ids = Tensor::from_vec(ids, (1, n), &device).unwrap();
            let hidden = model.clone().forward(&ids, 0).unwrap();
            let v: Vec<f32> = hidden
                .squeeze(0)
                .unwrap()
                .get(n - 1)
                .unwrap()
                .to_vec1()
                .unwrap();
            elitea_local_embed::vector::truncate(v, QWEN3_EMBEDDING_0_6B.dimension)
        })
        .collect()
}

#[test]
fn qwen3_pads_without_changing_vectors_and_ranks_the_answer_first() {
    let Some(root) = models() else { return };
    let dir = root.join("qwen3");
    if !dir.is_dir() {
        return;
    }
    let expected = reference_qwen3(&dir, &PASSAGES);
    let embedder =
        CandleEmbedder::load(&QWEN3_EMBEDDING_0_6B, &dir, DevicePreference::Auto).unwrap();
    let batched = embedder.embed_documents(&PASSAGES).unwrap();
    for (got, want) in batched.iter().zip(&expected) {
        let c = cosine(got, want);
        assert!(
            c > 0.995,
            "{}: cosine {c} with the reference",
            embedder.device_name()
        );
    }
    for (i, passage) in PASSAGES.iter().enumerate() {
        let alone = embedder.embed_documents(&[passage]).unwrap().remove(0);
        assert_eq!(alone.len(), QWEN3_EMBEDDING_0_6B.dimension);
        let c = cosine(&alone, &batched[i]);
        assert!(c > 0.995, "passage {i}: cosine {c}");
    }
    let query = embedder
        .embed_query("when does background embedding stop")
        .unwrap();
    let scores: Vec<f32> = batched.iter().map(|p| cosine(&query, p)).collect();
    assert!(scores[1] > scores[0] && scores[1] > scores[2], "{scores:?}");
}
