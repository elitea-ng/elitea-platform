"""A wiki whose source is an artifact folder.

These tests cover the three pieces the feature is made of: the materialiser
(``artifact_source``), the fifth branch in the ``repo_config`` extractor, and
the two early branches the engine's ``LocalRepositoryManager`` gained. The
last of those is tested through the REAL frozen class rather than a copy of
its logic, because the whole point of the branch is where it sits.
"""

from __future__ import annotations

import json
import shutil
from pathlib import Path

import pytest

from elitea_deepwiki.artifact_source import (
    ARTIFACT_SCHEME,
    ArtifactSource,
    ArtifactSourceError,
    artifact_repository_info,
    check_caps,
    child_environment,
    collect_objects,
    is_artifact_source,
    listing_digest,
    materialise_artifact_source,
    parse_artifact_source,
    repo_config_for,
    source_from_configuration,
)
from elitea_deepwiki.repo_config import _extract_repo_config_from_toolkit


class FakeArtifactClient:
    """The two methods the materialiser uses, and a record of the calls."""

    def __init__(self, objects: dict[str, bytes], modified: str = "2026-09-06T00:00:00Z"):
        self.objects = objects
        self.modified = modified
        self.listed: list[tuple[str, str]] = []
        self.downloaded: list[str] = []

    def list_artifacts(self, bucket, prefix=""):
        self.listed.append((bucket, prefix))
        return [
            {"name": key, "size": len(data), "modified": self.modified}
            for key, data in self.objects.items()
            if key.startswith(prefix)
        ]

    def download_artifact(self, bucket, name):
        self.downloaded.append(name)
        return self.objects[name]


# ---------------------------------------------------------------------------
# Parsing
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "raw,bucket,prefix",
    [
        ("artifact://docs-bucket", "docs-bucket", ""),
        ("artifact://docs-bucket/", "docs-bucket", ""),
        ("artifact://docs-bucket/handbook", "docs-bucket", "handbook"),
        ("artifact://docs-bucket/handbook/", "docs-bucket", "handbook"),
        ("  ARTIFACT://Docs-Bucket/a/b  ", "docs-bucket", "a/b"),
    ],
)
def test_a_source_is_parsed_out_of_the_repository_string(raw, bucket, prefix):
    source = parse_artifact_source(raw)
    assert (source.bucket, source.prefix) == (bucket, prefix)
    assert source.url == ARTIFACT_SCHEME + bucket + (f"/{prefix}" if prefix else "")


@pytest.mark.parametrize(
    "raw",
    [
        "owner/repo",
        "https://github.com/owner/repo",
        "",
        None,
    ],
)
def test_a_git_repository_is_not_an_artifact_source(raw):
    assert is_artifact_source(raw) is False
    with pytest.raises(ArtifactSourceError):
        parse_artifact_source(raw or "")


@pytest.mark.parametrize(
    "raw",
    [
        "artifact://Bucket_Name",  # underscores and capitals are not bucket names
        "artifact://9leading",
        "artifact://a",
        "artifact://",
        "artifact://good-bucket/../escape",
        "artifact://good-bucket/a//b",
    ],
)
def test_a_malformed_source_is_refused_before_any_credential_is_read(raw):
    """elitea-main's own bucket and key rules, applied here.

    The refusal must happen at parse time: the next step opens a platform
    client with a bearer token, and a source nobody could name should never
    get that far.
    """
    with pytest.raises(ArtifactSourceError):
        parse_artifact_source(raw)


def test_the_listing_prefix_is_a_folder_not_a_string_prefix():
    """`docs` must not match `docs-archive/`.

    The listing route applies the prefix server-side as plain text, so the
    trailing slash is the only thing that makes it a folder.
    """
    assert ArtifactSource("b", "docs").list_prefix == "docs/"
    assert ArtifactSource("b").list_prefix == ""


# ---------------------------------------------------------------------------
# The identity
# ---------------------------------------------------------------------------


def test_the_digest_ignores_listing_order_but_not_content():
    source = ArtifactSource("b", "docs")
    forward = collect_objects(
        source,
        [
            {"name": "docs/a.md", "size": 3, "modified": "t1"},
            {"name": "docs/b.md", "size": 4, "modified": "t2"},
        ],
    )
    backward = collect_objects(
        source,
        [
            {"name": "docs/b.md", "size": 4, "modified": "t2"},
            {"name": "docs/a.md", "size": 3, "modified": "t1"},
        ],
    )
    changed = collect_objects(
        source,
        [
            {"name": "docs/a.md", "size": 3, "modified": "t1"},
            {"name": "docs/b.md", "size": 5, "modified": "t2"},
        ],
    )
    assert listing_digest(forward) == listing_digest(backward)
    assert listing_digest(forward) != listing_digest(changed)


def test_a_rewritten_object_of_the_same_length_still_changes_the_digest():
    """The listing reports no content hash, so `modified` carries the change.

    Without it a file edited to the same length would reuse the previous
    index and the wiki would silently describe the old content.
    """
    source = ArtifactSource("b")
    before = collect_objects(source, [{"name": "a.md", "size": 3, "modified": "t1"}])
    after = collect_objects(source, [{"name": "a.md", "size": 3, "modified": "t2"}])
    assert listing_digest(before) != listing_digest(after)


def test_a_key_outside_the_folder_is_refused_not_skipped():
    with pytest.raises(ArtifactSourceError):
        collect_objects(ArtifactSource("b", "docs"), [{"name": "other/a.md", "size": 1}])


@pytest.mark.parametrize(
    "key",
    ["docs/../../etc/passwd", "docs/./a.md", "docs/a\\b.md", "docs//a.md"],
)
def test_a_key_that_escapes_or_is_unsafe_is_refused(key):
    with pytest.raises(ArtifactSourceError):
        collect_objects(ArtifactSource("b", "docs"), [{"name": key, "size": 1}])


def test_a_folder_placeholder_is_not_an_object_to_index():
    objects = collect_objects(
        ArtifactSource("b", "docs"),
        [{"name": "docs/", "size": 0}, {"name": "docs/a.md", "size": 1}],
    )
    assert [entry.relative_path for entry in objects] == ["a.md"]


def test_an_empty_folder_is_refused_with_a_reason():
    with pytest.raises(ArtifactSourceError, match="no objects"):
        check_caps([], ArtifactSource("b", "docs"))


def test_the_caps_refuse_a_folder_that_is_too_large(monkeypatch):
    objects = collect_objects(
        ArtifactSource("b"),
        [{"name": f"{index}.md", "size": 10} for index in range(5)],
    )
    monkeypatch.setenv("ELITEA_DEEPWIKI_ARTIFACT_MAX_FILES", "3")
    with pytest.raises(ArtifactSourceError, match="over the limit of 3"):
        check_caps(objects, ArtifactSource("b"))

    monkeypatch.delenv("ELITEA_DEEPWIKI_ARTIFACT_MAX_FILES")
    monkeypatch.setenv("ELITEA_DEEPWIKI_ARTIFACT_MAX_BYTES", "20")
    with pytest.raises(ArtifactSourceError, match="over the limit of 20"):
        check_caps(objects, ArtifactSource("b"))


def test_an_unreadable_cap_falls_back_to_the_default(monkeypatch):
    monkeypatch.setenv("ELITEA_DEEPWIKI_ARTIFACT_MAX_FILES", "not a number")
    objects = collect_objects(ArtifactSource("b"), [{"name": "a.md", "size": 1}])
    check_caps(objects, ArtifactSource("b"))


# ---------------------------------------------------------------------------
# Materialising
# ---------------------------------------------------------------------------


def test_the_folder_is_written_into_a_clone_shaped_directory(tmp_path):
    client = FakeArtifactClient(
        {
            "handbook/README.md": b"# Handbook\n",
            "handbook/guide/setup.md": b"install it\n",
            "elsewhere/ignored.md": b"not in the folder\n",
        }
    )
    path = materialise_artifact_source(
        "artifact://docs-bucket/handbook", "main", str(tmp_path), client=client
    )
    root = Path(path)
    assert (root / "README.md").read_bytes() == b"# Handbook\n"
    assert (root / "guide" / "setup.md").read_bytes() == b"install it\n"
    assert not (root / "ignored.md").exists()
    # The prefix is applied by the SERVER; the listing call says so.
    assert client.listed == [("docs-bucket", "handbook/")]


def test_the_identity_is_in_the_path_so_a_changed_folder_regenerates(tmp_path):
    first = materialise_artifact_source(
        "artifact://docs-bucket/handbook",
        "main",
        str(tmp_path),
        client=FakeArtifactClient({"handbook/a.md": b"one"}),
    )
    second = materialise_artifact_source(
        "artifact://docs-bucket/handbook",
        "main",
        str(tmp_path),
        client=FakeArtifactClient({"handbook/a.md": b"one", "handbook/b.md": b"two"}),
    )
    assert first != second, "a changed folder must not reuse the previous cache path"


def test_an_unchanged_folder_is_reused_without_downloading_again(tmp_path):
    objects = {"handbook/a.md": b"one"}
    first_client = FakeArtifactClient(objects)
    path = materialise_artifact_source(
        "artifact://docs-bucket/handbook", "main", str(tmp_path), client=first_client
    )
    second_client = FakeArtifactClient(objects)
    again = materialise_artifact_source(
        "artifact://docs-bucket/handbook", "main", str(tmp_path), client=second_client
    )
    assert again == path
    assert second_client.downloaded == []


def test_force_redownloads_the_same_folder(tmp_path):
    objects = {"handbook/a.md": b"one"}
    materialise_artifact_source(
        "artifact://docs-bucket/handbook",
        "main",
        str(tmp_path),
        client=FakeArtifactClient(objects),
    )
    client = FakeArtifactClient(objects)
    materialise_artifact_source(
        "artifact://docs-bucket/handbook",
        "main",
        str(tmp_path),
        client=client,
        force=True,
    )
    assert client.downloaded == ["handbook/a.md"]


def test_an_interrupted_download_is_not_reused(tmp_path):
    """The marker is written LAST, and it is what makes a cache hit.

    A directory without one is a half-written download, and reusing it would
    index a folder that is missing whatever the interruption dropped.
    """
    objects = {"handbook/a.md": b"one"}
    path = materialise_artifact_source(
        "artifact://docs-bucket/handbook",
        "main",
        str(tmp_path),
        client=FakeArtifactClient(objects),
    )
    Path(path + ".source.json").unlink()
    client = FakeArtifactClient(objects)
    materialise_artifact_source(
        "artifact://docs-bucket/handbook", "main", str(tmp_path), client=client
    )
    assert client.downloaded == ["handbook/a.md"]


def test_the_marker_is_beside_the_directory_and_not_inside_it(tmp_path):
    """A marker inside the tree would be discovered and indexed as content."""
    path = materialise_artifact_source(
        "artifact://docs-bucket/handbook",
        "main",
        str(tmp_path),
        client=FakeArtifactClient({"handbook/a.md": b"one"}),
    )
    assert Path(path + ".source.json").is_file()
    assert sorted(entry.name for entry in Path(path).iterdir()) == ["a.md"]


def test_a_version_label_cannot_choose_the_directory(tmp_path):
    """The branch is a caller's string and it lands in a path.

    A label holding `..` would otherwise pick the directory that gets written
    — and, on a forced re-download, removed.
    """
    path = materialise_artifact_source(
        "artifact://docs-bucket/handbook",
        "../../escape",
        str(tmp_path),
        client=FakeArtifactClient({"handbook/a.md": b"one"}),
    )
    # The separator is what makes a label dangerous, and it is folded away:
    # the destination is one directory directly under the cache.
    assert Path(path).parent.resolve() == tmp_path.resolve()
    assert Path(path).resolve().is_relative_to(tmp_path.resolve())


def test_the_repository_info_answers_with_a_content_derived_commit(tmp_path):
    path = materialise_artifact_source(
        "artifact://docs-bucket/handbook",
        "v3",
        str(tmp_path),
        client=FakeArtifactClient({"handbook/a.md": b"one"}),
    )
    info = artifact_repository_info(path)
    assert info["branch"] == "v3"
    assert info["remote_url"] == "artifact://docs-bucket/handbook"
    assert info["exists"] is True
    assert len(info["commit_hash"]) == 64
    # The path carries the same identity, which is what makes the cache key
    # and the clone directory agree.
    assert Path(path).name.endswith(info["commit_hash"][:8])


def test_a_git_clone_has_no_artifact_marker(tmp_path):
    assert artifact_repository_info(str(tmp_path / "some_clone")) is None


def test_an_unreadable_marker_reads_as_a_clone(tmp_path):
    (tmp_path / "clone.source.json").write_text("{not json")
    assert artifact_repository_info(str(tmp_path / "clone")) is None


def test_a_missing_credential_says_what_is_missing(monkeypatch):
    for name in (
        "DEEPWIKI_ARTIFACT_BASE_URL",
        "DEEPWIKI_ARTIFACT_API_KEY",
        "DEEPWIKI_ARTIFACT_PROJECT_ID",
    ):
        monkeypatch.delenv(name, raising=False)
    with pytest.raises(ArtifactSourceError, match="platform artifact credentials"):
        materialise_artifact_source("artifact://docs-bucket", "main", "/tmp/nowhere")


# ---------------------------------------------------------------------------
# The child environment
# ---------------------------------------------------------------------------


LLM_SETTINGS = {
    "api_base": "https://elitea.example/llm/v1",
    "api_key": "callback-bearer",
    "organization": 90200,
}


def test_a_git_generation_carries_no_platform_bearer():
    payload = {
        "repo_config": {"provider_type": "github", "repository": "owner/repo"},
        "llm_settings": LLM_SETTINGS,
    }
    assert child_environment(payload) == {}


def test_an_artifact_generation_carries_the_invocations_own_credentials():
    payload = {
        "repo_config": {
            "provider_type": "artifact",
            "repository": "artifact://docs-bucket/handbook",
        },
        "llm_settings": LLM_SETTINGS,
    }
    environment = child_environment(payload)
    assert environment["DEEPWIKI_ARTIFACT_BASE_URL"] == "https://elitea.example"
    assert environment["DEEPWIKI_ARTIFACT_API_KEY"] == "callback-bearer"
    assert environment["DEEPWIKI_ARTIFACT_PROJECT_ID"] == "90200"
    # The wiki's OWN artifacts keep their own bucket: writing the generated
    # wiki back into the folder it was made from would corrupt the source.
    assert environment["DEEPWIKI_ARTIFACT_BUCKET"] == "wiki-artifacts"


def test_a_payload_without_credentials_carries_nothing():
    payload = {
        "repo_config": {"repository": "artifact://docs-bucket"},
        "llm_settings": {"api_base": "", "api_key": ""},
    }
    assert child_environment(payload) == {}
    assert child_environment(None) == {}


# ---------------------------------------------------------------------------
# The fifth branch in the repo_config extractor
# ---------------------------------------------------------------------------


def test_the_extractor_reads_an_artifact_configuration_block():
    config = _extract_repo_config_from_toolkit(
        {
            "code_toolkit": {
                "artifact_configuration": {"bucket": "docs-bucket", "prefix": "handbook/"},
                "active_branch": "v2",
            }
        }
    )
    assert config == {
        "provider_type": "artifact",
        "provider_config": {"bucket": "docs-bucket", "prefix": "handbook"},
        "repository": "artifact://docs-bucket/handbook",
        "branch": "v2",
        "project": None,
        "is_cloud": None,
    }


def test_the_extractor_reads_the_prefixed_spelling_too():
    config = _extract_repo_config_from_toolkit(
        {
            "toolkit_configuration_code_toolkit": {
                "toolkit_configuration_artifact_configuration": {"bucket": "docs-bucket"},
            }
        }
    )
    assert config["provider_type"] == "artifact"
    assert config["repository"] == "artifact://docs-bucket"
    assert config["branch"] == "main"


def test_the_extractor_reads_a_bare_artifact_repository_string():
    config = _extract_repo_config_from_toolkit(
        {"code_toolkit": {"repository": "artifact://docs-bucket/handbook"}}
    )
    assert config["provider_type"] == "artifact"
    assert config["provider_config"] == {"bucket": "docs-bucket", "prefix": "handbook"}


def test_the_github_path_is_unchanged_by_the_new_branch():
    """The legacy four providers keep their exact shape.

    The artifact branch returns early, so this is the assertion that says it
    cannot have changed anything on the way past.
    """
    config = _extract_repo_config_from_toolkit(
        {
            "code_toolkit": {
                "github_configuration": {"base_url": "https://api.github.com"},
                "repository": "owner/repo",
                "active_branch": "trunk",
            }
        }
    )
    assert config == {
        "provider_type": "github",
        "provider_config": {"base_url": "https://api.github.com"},
        "repository": "owner/repo",
        "branch": "trunk",
        "project": None,
        "is_cloud": None,
    }


def test_a_source_can_be_built_from_a_configuration_block():
    assert source_from_configuration(
        {"bucket": "docs-bucket", "prefix": "/docs/"}
    ) == ArtifactSource("docs-bucket", "docs")
    assert source_from_configuration(
        {"bucket": "docs-bucket", "folder": "docs"}
    ) == ArtifactSource("docs-bucket", "docs")
    assert source_from_configuration({"bucket": ""}) is None
    assert source_from_configuration(None) is None


def test_the_repo_config_shape_matches_the_extractor():
    assert repo_config_for(ArtifactSource("b", "docs"), "") == {
        "provider_type": "artifact",
        "provider_config": {"bucket": "b", "prefix": "docs"},
        "repository": "artifact://b/docs",
        "branch": "main",
        "project": None,
        "is_cloud": None,
    }


# ---------------------------------------------------------------------------
# The two branches inside the frozen engine
# ---------------------------------------------------------------------------


def test_the_engine_manager_materialises_instead_of_cloning(tmp_path, monkeypatch):
    """The seam, exercised through the real frozen class.

    ``get_repository_local_path`` would otherwise reach ``git ls-remote`` and
    ``git clone``; the branch returns before ``_get_clone_config``, which
    ``GitCloneConfig`` would refuse for a source with no clone URL.
    """
    from elitea_deepwiki.engine.local_repository_manager import LocalRepositoryManager

    client = FakeArtifactClient({"handbook/a.md": b"one"})
    monkeypatch.setattr(
        "elitea_deepwiki.artifact_source.artifact_client", lambda: client
    )
    manager = LocalRepositoryManager(cache_dir=str(tmp_path))

    path = manager.get_repository_local_path("artifact://docs-bucket/handbook", "main")

    assert Path(path, "a.md").read_bytes() == b"one"
    assert manager.active_repos == {"artifact://docs-bucket/handbook:main": path}

    # And the second branch: git would fail in a directory that is not a
    # repository, leaving no commit hash and a cache key of "unknown".
    info = manager.get_repository_info(path)
    assert info["commit_hash"] and info["branch"] == "main"
    assert info["remote_url"] == "artifact://docs-bucket/handbook"


def test_the_engine_manager_still_takes_the_clone_path_for_a_git_source(tmp_path):
    """A git repository must not reach the artifact branch at all."""
    from elitea_deepwiki.engine.local_repository_manager import LocalRepositoryManager

    manager = LocalRepositoryManager(cache_dir=str(tmp_path))
    calls: list[tuple] = []

    def refuse(*args, **kwargs):
        calls.append((args, kwargs))
        raise RuntimeError("clone path reached")

    manager._get_clone_config = refuse
    with pytest.raises(RuntimeError, match="clone path reached"):
        manager.get_repository_local_path("owner/repo", "main")
    assert calls, "the git source must reach _get_clone_config"


def test_the_manifest_declares_the_engine_branches():
    """The frozen file's edit is declared, not silent.

    ``tests/engine/test_copy_is_verbatim.py`` enforces the digests; this
    asserts the DECLARATION exists, so the branch cannot be moved out of the
    manifest and left in the code.
    """
    manifest = json.loads(
        (
            Path(__file__).resolve().parents[2]
            / "src"
            / "elitea_deepwiki"
            / "engine"
            / "COPY_MANIFEST.json"
        ).read_text(encoding="utf-8")
    )
    entry = manifest["transformed_files"]["plugin_implementation/local_repository_manager.py"]
    assert entry["in_place"] is True
    assert len(entry["substitutions"]) == 2
    assert any("materialise_artifact_source" in item["to"] for item in entry["substitutions"])
    assert any("artifact_repository_info" in item["to"] for item in entry["substitutions"])


def test_the_subprocess_launchers_pass_the_child_environment():
    """All three launchers, because the substitution is one replace over three.

    The generated tool layer is what actually runs; asserting on the source
    is how a lost substitution is caught without launching a worker.
    """
    source = (
        Path(__file__).resolve().parents[2]
        / "src"
        / "elitea_deepwiki"
        / "tool_operations.py"
    ).read_text(encoding="utf-8")
    assert source.count("env.update(child_environment(payload))") == 3


def test_the_python_fixture_runner_names_a_folder_source_like_the_go_one():
    """The two fixture runners serve the same journeys on different stacks.

    The E2E stack runs the Go one (``run/fixture.go``), the standalone stack
    runs this one, and DWIKI-018 asserts the same wiki id and the same
    manifest against both. They are kept in step by hand, so the assertion
    below is the same one the Go test makes.
    """
    from elitea_deepwiki import fixture_runner
    from elitea_deepwiki.wiki_context import display_repository_for, wiki_id_for

    repo_config = {
        "provider_type": "artifact",
        "repository": "artifact://handbook-bucket/docs",
    }
    assert display_repository_for(repo_config) == "handbook-bucket/docs"
    assert wiki_id_for(repo_config, "main") == "handbook-bucket--docs--main"
    # And a git source is named exactly as it was.
    assert wiki_id_for({"repository": "acme/notes"}, "main") == "acme--notes--main"

    result = fixture_runner.generate_wiki(
        query="Document the handbook", repo_config=repo_config, active_branch="main"
    )
    assert result["wiki_id"] == "handbook-bucket--docs--main"
    manifest = json.loads(
        next(
            entry["data"]
            for entry in result["artifacts"]
            if "wiki_manifest_" in entry["name"]
        )
    )
    assert manifest["repository"] == "handbook-bucket/docs"
    assert manifest["provider_type"] == "artifact"
    assert manifest["wiki_title"] == "docs wiki"


def test_the_jobs_runner_cannot_carry_the_artifact_credential_yet():
    """A stated gap, so it cannot be found later as a surprise.

    ``_job_env`` drops every forwarded name ending in ``_KEY`` — ADR-0022
    wants a projected file — and nothing reads the credentials directory yet.
    So a Job pod gets no artifact bearer, which is why an artifact source is a
    subprocess-runner feature today. A Job cannot upload a wiki either; the
    two are one piece of work.
    """
    from elitea_deepwiki.jobs import _is_secret_env

    assert _is_secret_env("DEEPWIKI_ARTIFACT_API_KEY") is True
    assert _is_secret_env("DEEPWIKI_ARTIFACT_BASE_URL") is False


def test_a_payload_that_is_not_an_object_names_no_artifact_source():
    from elitea_deepwiki.repo_config import _extract_artifact_source

    assert _extract_artifact_source(None) is None
    assert _extract_artifact_source("artifact://docs-bucket") is None


def test_a_key_longer_than_the_platform_allows_is_refused():
    with pytest.raises(ArtifactSourceError, match="1024 bytes"):
        parse_artifact_source("artifact://docs-bucket/" + "a" * 1100)


def test_a_size_the_listing_could_not_report_counts_as_zero():
    """A listing entry with no usable size must not stop a generation.

    The size feeds the byte cap and the digest. An unreadable one is a
    server that answered oddly, not a folder to refuse.
    """
    objects = collect_objects(
        ArtifactSource("docs-bucket"), [{"name": "a.md", "size": "not a number"}]
    )
    assert objects[0].size == 0


def test_a_stale_marker_beside_a_missing_directory_is_replaced(tmp_path):
    objects = {"handbook/a.md": b"one"}
    path = materialise_artifact_source(
        "artifact://docs-bucket/handbook",
        "main",
        str(tmp_path),
        client=FakeArtifactClient(objects),
    )
    shutil.rmtree(path)
    client = FakeArtifactClient(objects)
    again = materialise_artifact_source(
        "artifact://docs-bucket/handbook", "main", str(tmp_path), client=client
    )
    assert again == path
    assert client.downloaded == ["handbook/a.md"]
    assert Path(path, "a.md").read_bytes() == b"one"
