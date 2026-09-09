"""Worker-level dispatch proof for the imagegen toolkit (#864).

Before the elitea-sdk pin carried the imagegen patch, ``elitea_sdk.tools``
never registered ``imagegen`` at all — the tile was served (elitea-main's
hand-written catalogue entry) but a real dispatch through
``EliteaSdkToolkitToolAdapter.call_tool`` (the exact method the production
worker uses for ``toolkit.call_tool.v1``, which delegates to the SDK's own
``EliteAClient.test_toolkit_tool``) failed to build the toolkit at all and
answered "cannot be built".

This test drives that SAME adapter, against the REAL admitted+patched SDK
installed in this environment (not a stand-in), with only the outbound HTTP
calls stubbed (the images-generation upstream and the artifact bucket/S3
calls) — everything else (config validation, toolkit instantiation via
``elitea_sdk.runtime.toolkits.tools.get_toolkits``, tool-name resolution,
``ImageGenAPIWrapper.generate_image``, base64 decode, and the artifact
``create`` call) is the real code path. A run against the admitted SDK
without the imagegen patch fails to import the toolkit at all, so a green run
here is the dispatch proof issue #864 asks for: a request for
``generate_image`` ends with an artifact recorded in the configured bucket.
"""

from __future__ import annotations

import base64
import json
from typing import Any
from unittest.mock import patch

import pytest

from elitea_worker.agents.sdk_adapter import EliteaSdkToolkitToolAdapter

_TINY_PNG = base64.b64encode(b"\x89PNG\r\n\x1a\nfake-image-bytes").decode()


class _FakeResponse:
    def __init__(self, status_code: int = 200, json_body: Any = None, content: bytes = b""):
        self.status_code = status_code
        self._json_body = json_body
        self.content = content
        self.text = json.dumps(json_body) if json_body is not None else ""

    def json(self) -> Any:
        return self._json_body

    def raise_for_status(self) -> None:
        if self.status_code >= 400:
            raise RuntimeError(f"HTTP {self.status_code}")


def _fake_requests_get(url: str, headers=None, verify=None, params=None, timeout=None, **kwargs):
    if url.endswith("/artifacts/buckets/1"):
        # bucket_exists(): the toolkit's configured bucket already exists, so
        # Artifact.__init__ skips create_bucket.
        return _FakeResponse(200, {"rows": [{"name": "generated-images"}]})
    raise AssertionError(f"unexpected GET {url}")


def _fake_requests_post(url: str, headers=None, json=None, data=None, files=None, verify=None, timeout=None, **kwargs):
    if url.endswith("/llm/v1/images/generations"):
        # This is the exact route the gateway serves
        # (services/elitea-llm-gateway/internal/llmproxy/handler.go:783) and
        # deploy/mock-llm/server.py's `_images_generations` stub answers, in
        # the same b64_json response_format the wrapper requests.
        assert json is not None
        assert json["model"] == "gpt-image-1"
        assert json["prompt"] == "a red circle on a white background"
        return _FakeResponse(200, {"data": [{"b64_json": _TINY_PNG}]})
    raise AssertionError(f"unexpected POST {url}")


def _fake_requests_put(url: str, headers=None, data=None, params=None, verify=None, timeout=None, **kwargs):
    if "/artifacts/s3/generated-images/" in url:
        # upload_artifact_s3(): the artifact write. Its success is the "ends
        # with an artifact in the bucket" proof — the PUT target names the
        # exact bucket the toolkit was configured with.
        return _FakeResponse(200)
    raise AssertionError(f"unexpected PUT {url}")


@pytest.fixture
def _real_admitted_sdk_client():
    try:
        from elitea_sdk.runtime.clients.client import EliteAClient
    except ModuleNotFoundError:
        pytest.skip("the admitted elitea-sdk artifact is not installed in this environment")
    return EliteAClient(base_url="https://gateway.internal", project_id=1, auth_token="test-token")


def test_generate_image_dispatches_through_the_real_worker_adapter_and_saves_an_artifact(
    _real_admitted_sdk_client,
) -> None:
    client = _real_admitted_sdk_client
    adapter = EliteaSdkToolkitToolAdapter(client)

    toolkit_config = {
        "toolkit_name": "imagegen",
        "settings": {
            "image_generation_model": "gpt-image-1",
            "bucket": "generated-images",
            "name_prefix": "test-",
        },
    }

    with (
        patch("elitea_sdk.runtime.clients.client.requests.get", side_effect=_fake_requests_get),
        patch("elitea_sdk.runtime.clients.client.requests.post", side_effect=_fake_requests_post),
        patch("elitea_sdk.runtime.clients.client.requests.put", side_effect=_fake_requests_put),
        patch("elitea_sdk.tools.imagegen.api_wrapper.requests.post", side_effect=_fake_requests_post),
    ):
        result = adapter.call_tool(
            toolkit_config=toolkit_config,
            tool_name="generate_image",
            tool_params={"prompt": "a red circle on a white background"},
            runtime_config={},
            llm_model="gpt-4o-mini",
            llm_config={},
            mcp_tokens=None,
        )

    assert result["success"] is True, result.get("error")
    payload = result["result"]
    if isinstance(payload, str):
        payload = json.loads(payload)
    artifacts = payload["artifacts"]
    assert len(artifacts) == 1
    assert artifacts[0]["filepath"] == "/generated-images/test-generate-0.png"


def test_edit_image_dispatches_through_the_real_worker_adapter_and_saves_an_artifact(
    _real_admitted_sdk_client,
) -> None:
    client = _real_admitted_sdk_client
    adapter = EliteaSdkToolkitToolAdapter(client)

    toolkit_config = {
        "toolkit_name": "imagegen",
        "settings": {
            "image_generation_model": "gpt-image-1",
            "bucket": "generated-images",
            "name_prefix": "test-",
        },
    }

    def fake_get_for_edit(url: str, headers=None, verify=None, params=None, timeout=None, **kwargs):
        if url.endswith("/artifacts/buckets/1"):
            return _FakeResponse(200, {"rows": [{"name": "generated-images"}]})
        if url.endswith("/artifacts/artifact/default/1/generated-images/source.png"):
            # get_raw_content_by_filepath() -> download_artifact_by_filepath()
            # -> download_artifact(), which reads response.content directly.
            return _FakeResponse(200, content=b"source-bytes")
        raise AssertionError(f"unexpected GET {url}")

    def fake_post_for_edit(url: str, headers=None, json=None, data=None, files=None, verify=None, timeout=None, **kwargs):
        if url.endswith("/llm/v1/images/edits"):
            assert files is not None and "image" in files
            assert data["prompt"] == "add a blue border"
            return _FakeResponse(200, {"data": [{"b64_json": _TINY_PNG}]})
        raise AssertionError(f"unexpected POST {url}")

    with (
        patch("elitea_sdk.runtime.clients.client.requests.get", side_effect=fake_get_for_edit),
        patch("elitea_sdk.runtime.clients.client.requests.put", side_effect=_fake_requests_put),
        patch("elitea_sdk.tools.imagegen.api_wrapper.requests.post", side_effect=fake_post_for_edit),
    ):
        result = adapter.call_tool(
            toolkit_config=toolkit_config,
            tool_name="edit_image",
            tool_params={"image": "/generated-images/source.png", "prompt": "add a blue border"},
            runtime_config={},
            llm_model="gpt-4o-mini",
            llm_config={},
            mcp_tokens=None,
        )

    assert result["success"] is True, result.get("error")
    payload = result["result"]
    if isinstance(payload, str):
        payload = json.loads(payload)
    artifacts = payload["artifacts"]
    assert len(artifacts) == 1
    assert artifacts[0]["filepath"] == "/generated-images/test-edit-0.png"
