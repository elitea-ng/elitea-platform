# Python tokenizer cache under runtime isolation

The Python worker validates its indexing dependencies before it starts the command consumer.
The Markdown check uses the current SDK parser and its cl100k tokenizer.
The tokenizer must load its admitted public asset without a startup download.

## Source mapping

| Reference behavior | Source | Worker behavior |
| --- | --- | --- |
| Markdown parsing selects EliteAMarkdownLoader. | SDK `tools/utils/content_parser.py`, `process_content_by_type` | Preserve the selected parser. |
| Markdown chunks require the cl100k tokenizer. | SDK `tools/chunkers/utils.py`, `tiktoken_length` | Preserve token counting. |
| A valid cached asset returns before HTTP. | tiktoken 0.14.0 `load.py`, `read_file_cached` | Keep the verified public asset in an image directory. |
| Startup requires the complete indexing profile. | `services/elitea-worker-python/src/elitea_worker/indexing_runtime_capabilities.py` | Preserve the admission check. |
| The image previously warmed the default cache under /tmp. | `services/elitea-worker-python/Containerfile` | Warm `/opt/elitea/tokenizer-cache` before build admission. |
| Runtime isolation overlays /tmp and retains a read-only root. | Fresh deployed worker configuration | Read the cache outside the temporary mount. |

The selected SDK revision is b5113a129329b85d23c2d5c2bf55f18e307414ec with its five existing lock patches.
This change does not modify the SDK, dependency pins, capability profile, or parser.
The image sets TIKTOKEN_CACHE_DIR before its existing build checks.
The image gives the runtime user read access to those public assets.
Both capability checks also run as UID 10001 with build networking disabled.

## Failure evidence

The isolated worker exits 5 before it reads runtime credentials or opens the command consumer.
The full capability diagnostic identifies Markdown as the only failing category.
The direct Markdown diagnostic finds no cl100k cache leaf under the mounted /tmp.
Its traceback reaches read_file_cached, requests, and NameResolutionError through the current SDK parser.
The diagnostic preserves the worker's non-root user, read-only root, temporary mount, and network restrictions.

The capability diagnostic receipt hash is `1f20dd3726f268badfba35506711bd776c787aa31b99e7f8f7d51a315f10d08c`.
The direct Markdown receipt hash is `f089ed79c4b338e69db605c4f1c8fc787004168187f2998f91b9d0fceed727c9`.

Readback from the stopped image finds the original 1,681,126-byte public asset.
Its SHA-256 matches tiktoken's required content hash: `223921b76ee99bde995b7ff738513eef100fb51d18c93597a113bcffe865b2a7`.
The baked asset readback receipt hash is `195ba920fab9d29e6987f66ce7f11638dd881e2f325e54d5e1b4d24026e76881`.
These receipts establish the failure cause. They do not establish successful corrected startup.

## Implementation history and limits

The original root build admission passed with a cache hidden by the later runtime mount.
The packaging change moves that cache into the image's stable filesystem.
The fresh-stack correction uses the same image asset through a supplementary read-only mount.
The frozen image acceptance and the rebuilt image acceptance require separate receipts.
The focused SDK identity check passes after this source change.
The corrected image build and deployed Code execution acceptance remain pending.

The supplementary cache correction passes the complete unchanged indexing profile under the original runtime restrictions.
The new Python service retains the same PID through its 15-second startup observation.
The correction receipt hash is `9e76a27742a4372674fe8c43cae48a88efbd7bff768c6a254eca474f9ae5cce6`.
This bounded startup check does not establish a functional execution or a rebuilt image.
