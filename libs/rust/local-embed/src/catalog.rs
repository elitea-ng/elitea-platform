//! The models a person can choose to embed their index with on this
//! computer. Nothing is downloaded until they choose one.
//!
//! Every file is pinned by SHA-256 (the Hugging Face LFS object id, the same
//! digest the CDN's `x-linked-etag` carries) at a fixed repository commit:
//! the download is verified against these digests before anything loads it,
//! and a file imported by hand must match them too.

use serde::Serialize;

/// How a model turns its last hidden states into one vector.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Family {
    /// BERT-style encoder, mean pooling over the attention mask
    /// (multilingual-e5).
    Bert,
    /// Qwen3 decoder, last-token pooling (Qwen3-Embedding).
    Qwen3,
}

/// One file of a model.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
pub struct ModelFile {
    pub name: &'static str,
    /// Lowercase hex SHA-256 of the file's bytes.
    pub sha256: &'static str,
    pub size: u64,
}

/// One model of the catalogue.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
pub struct ModelSpec {
    /// What settings store (`local:<id>`).
    pub id: &'static str,
    pub display_name: &'static str,
    pub family: Family,
    /// The Hugging Face repository and the commit every file is read at.
    pub repo: &'static str,
    pub revision: &'static str,
    /// The weights first: its digest names the model's cache directory.
    pub files: &'static [ModelFile],
    /// The model's own output width.
    pub native_dimension: usize,
    /// The width the index stores (Matryoshka truncation where the model
    /// was trained for it; the native width otherwise).
    pub dimension: usize,
    /// The longest chunk it reads, in its own tokens (longer ones are cut).
    pub max_tokens: usize,
    /// Roughly how much memory it takes while it embeds (measured on an
    /// M4 Pro, batches of 32; see the crate's `embed_bench` example).
    pub ram_mb: u32,
    /// A sentence for the settings page.
    pub summary: &'static str,
    pub license: &'static str,
}

impl ModelSpec {
    /// Bytes to download.
    #[must_use]
    pub fn download_bytes(&self) -> u64 {
        self.files.iter().map(|file| file.size).sum()
    }

    /// The digest that names its cache directory (the weights').
    #[must_use]
    pub fn cache_key(&self) -> &'static str {
        self.files.first().map_or("", |file| file.sha256)
    }

    /// The stamp its vectors carry.
    #[must_use]
    pub fn stamp(&self) -> String {
        format!("local:{}", self.id)
    }

    /// A query as the model was trained to read it.
    #[must_use]
    pub fn query_text(&self, query: &str) -> String {
        match self.family {
            Family::Bert => format!("query: {query}"),
            Family::Qwen3 => format!("Instruct: {QUERY_TASK}\nQuery:{query}"),
        }
    }

    /// A document chunk as the model was trained to read it.
    #[must_use]
    pub fn document_text(&self, text: &str) -> String {
        match self.family {
            Family::Bert => format!("passage: {text}"),
            Family::Qwen3 => text.to_owned(),
        }
    }

    /// The URL of one of its files, at the pinned commit, under `base`
    /// (`https://huggingface.co` or the organisation's mirror).
    #[must_use]
    pub fn file_url(&self, base: &str, file: &ModelFile) -> String {
        format!(
            "{}/{}/resolve/{}/{}",
            base.trim_end_matches('/'),
            self.repo,
            self.revision,
            file.name
        )
    }
}

/// The instruction Qwen3-Embedding queries carry: a workspace search finds
/// code and documents.
pub const QUERY_TASK: &str =
    "Given a question about a software project, retrieve the code and documentation that answer it";

/// The default download origin.
pub const HUGGING_FACE: &str = "https://huggingface.co";

/// Small, fast, 100 languages: the lighter choice.
pub const MULTILINGUAL_E5_SMALL: ModelSpec = ModelSpec {
    id: "multilingual-e5-small",
    display_name: "multilingual-e5-small",
    family: Family::Bert,
    repo: "intfloat/multilingual-e5-small",
    revision: "614241f622f53c4eeff9890bdc4f31cfecc418b3",
    files: &[
        ModelFile {
            name: "model.safetensors",
            sha256: "1a55775f53449dac10a2bcbc312469fac40b96d53198c407081a831f81c98477",
            size: 470_641_600,
        },
        ModelFile {
            name: "tokenizer.json",
            sha256: "0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39",
            size: 17_082_730,
        },
        ModelFile {
            name: "config.json",
            sha256: "69137736cab8b8903a07fe8afaafdda25aac55415a12a55d1bffa9f581abf959",
            size: 655,
        },
    ],
    native_dimension: 384,
    dimension: 384,
    max_tokens: 512,
    ram_mb: 1400,
    summary: "Small and fast (118M parameters, 384 dimensions); good for prose in many languages, weaker on code.",
    license: "MIT",
};

/// Larger, better on code and long queries.
pub const QWEN3_EMBEDDING_0_6B: ModelSpec = ModelSpec {
    id: "qwen3-embedding-0.6b",
    display_name: "Qwen3-Embedding-0.6B",
    family: Family::Qwen3,
    repo: "Qwen/Qwen3-Embedding-0.6B",
    revision: "97b0c614be4d77ee51c0cef4e5f07c00f9eb65b3",
    files: &[
        ModelFile {
            name: "model.safetensors",
            sha256: "0437e45c94563b09e13cb7a64478fc406947a93cb34a7e05870fc8dcd48e23fd",
            size: 1_191_586_416,
        },
        ModelFile {
            name: "tokenizer.json",
            sha256: "def76fb086971c7867b829c23a26261e38d9d74e02139253b38aeb9df8b4b50a",
            size: 11_423_705,
        },
        ModelFile {
            name: "config.json",
            sha256: "b5bf1f51fc45be473a54718cef92448d90a1be001bf9b9a44b8c7f10a19feaa9",
            size: 727,
        },
    ],
    native_dimension: 1024,
    // Matryoshka: trained to be truncated; 512 keeps nearly all of its
    // retrieval quality at half the size.
    dimension: 512,
    max_tokens: 512,
    ram_mb: 2600,
    summary: "Better on code and questions (600M parameters, stored at 512 dimensions); slower, needs more memory.",
    license: "Apache-2.0",
};

/// Every model, in the order the settings page offers them.
pub const MODELS: [&ModelSpec; 2] = [&MULTILINGUAL_E5_SMALL, &QWEN3_EMBEDDING_0_6B];

/// The model `id` names.
#[must_use]
pub fn model(id: &str) -> Option<&'static ModelSpec> {
    MODELS.iter().copied().find(|spec| spec.id == id)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn every_digest_is_sha256_hex_and_ids_are_unique() {
        for spec in MODELS {
            assert!(spec.dimension <= spec.native_dimension);
            assert_eq!(spec.files[0].name, "model.safetensors");
            for file in spec.files {
                assert_eq!(file.sha256.len(), 64, "{}", file.name);
                assert!(
                    file.sha256
                        .bytes()
                        .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
                );
            }
            assert_eq!(model(spec.id), Some(spec));
        }
        assert_ne!(MODELS[0].cache_key(), MODELS[1].cache_key());
        assert!(model("nope").is_none());
    }

    #[test]
    fn urls_are_pinned_to_the_commit() {
        let spec = &MULTILINGUAL_E5_SMALL;
        assert_eq!(
            spec.file_url("https://mirror.example/hf/", &spec.files[1]),
            "https://mirror.example/hf/intfloat/multilingual-e5-small/resolve/614241f622f53c4eeff9890bdc4f31cfecc418b3/tokenizer.json"
        );
        assert_eq!(spec.query_text("x"), "query: x");
        assert!(QWEN3_EMBEDDING_0_6B.query_text("x").ends_with("\nQuery:x"));
    }
}
