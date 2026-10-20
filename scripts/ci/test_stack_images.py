"""Tests for scripts/ci/stack_images.py, the build-once digest-map contract."""

from __future__ import annotations

import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import stack_images as si  # noqa: E402

WS = "/home/runner/work/elitea-platform/elitea-platform"
DIGEST = "sha256:" + "a" * 64


def config(**services):
    return {"services": services}


def svc(image, dockerfile, target=None, context=WS, args=None):
    build = {"context": context, "dockerfile": dockerfile}
    if target:
        build["target"] = target
    if args:
        build["args"] = args
    return {"image": image, "build": build}


MAIN_E2E = svc("ghcr.io/elitea-ng/elitea-main:standalone", "services/elitea-main/Containerfile", "e2e")
HOST = svc("ghcr.io/elitea-ng/elitea-subapp-host:standalone", "services/elitea-subapp-host/Containerfile")


class StackImagesTest(unittest.TestCase):
    def built(self, cfg, *services):
        entries, sets = si.plan(cfg, WS, "ghcr.io/elitea-ng/ci-", "deadbeef", list(services))
        meta = {e["service"]: {"containerimage.digest": DIGEST} for e in entries}
        return si.record(entries, meta), sets

    def test_plan_names_ci_images_after_the_compose_image(self):
        _, sets = self.built(config(**{"elitea-main": MAIN_E2E, "elitea-deepwiki": HOST}), "elitea-main", "elitea-deepwiki")
        self.assertEqual(
            sets,
            [
                "elitea-main.tags=ghcr.io/elitea-ng/ci-elitea-main:deadbeef",
                "elitea-deepwiki.tags=ghcr.io/elitea-ng/ci-elitea-subapp-host:deadbeef",
            ],
        )

    def test_one_build_serves_every_tag_of_the_same_variant(self):
        digests, _ = self.built(config(**{"elitea-main": MAIN_E2E}), "elitea-main")
        # The e2e compose tags the same build under another org and tag, and
        # three standalone services share it: one pull, every local tag.
        lane = config(
            **{
                "elitea-main": svc("ghcr.io/eliteaai/elitea-main:e2e", "services/elitea-main/Containerfile", "e2e"),
                "elitea-migrate": MAIN_E2E,
                "elitea-agentstate-migrate": MAIN_E2E,
            }
        )
        self.assertEqual(
            si.resolve(lane, WS, digests),
            [
                ("ghcr.io/elitea-ng/elitea-main:standalone", f"ghcr.io/elitea-ng/ci-elitea-main@{DIGEST}"),
                ("ghcr.io/eliteaai/elitea-main:e2e", f"ghcr.io/elitea-ng/ci-elitea-main@{DIGEST}"),
            ],
        )

    def test_a_different_variant_is_refused(self):
        digests, _ = self.built(config(**{"elitea-main": MAIN_E2E}), "elitea-main")
        for changed in (
            svc("x/elitea-main:final", "services/elitea-main/Containerfile", "final"),
            svc("x/elitea-main:e2e", "services/elitea-main/Containerfile", "e2e", args={"VERSION": "1"}),
            svc("x/elitea-main:e2e", "other/Containerfile", "e2e"),
        ):
            with self.assertRaisesRegex(si.StackImageError, "but this compose builds"):
                si.resolve(config(**{"elitea-main": changed}), WS, digests)

    def test_an_image_nobody_built_is_named(self):
        digests, _ = self.built(config(**{"elitea-main": MAIN_E2E}), "elitea-main")
        with self.assertRaisesRegex(si.StackImageError, "elitea-subapp-host was built"):
            si.resolve(config(**{"elitea-main": MAIN_E2E, "elitea-inventory": HOST}), WS, digests)

    def test_vacuous_inputs_fail(self):
        with self.assertRaisesRegex(si.StackImageError, "vacuous"):
            si.resolve(config(postgres={"image": "postgres:18"}), WS, {})
        with self.assertRaisesRegex(si.StackImageError, "not a `build:` service"):
            si.plan(config(postgres={"image": "postgres:18"}), WS, "p-", "t", ["postgres"])

    def test_the_same_image_twice_in_one_plan_is_refused(self):
        with self.assertRaisesRegex(si.StackImageError, "build it once"):
            si.plan(config(a=MAIN_E2E, b=MAIN_E2E), WS, "p-", "t", ["a", "b"])

    def test_a_build_without_a_digest_fails(self):
        entries, _ = si.plan(config(**{"elitea-main": MAIN_E2E}), WS, "p-", "t", ["elitea-main"])
        with self.assertRaisesRegex(si.StackImageError, "no pushed digest"):
            si.record(entries, {"elitea-main": {}})

    def test_merge_refuses_conflicting_fragments(self):
        a = {"x": {"ref": "r", "digest": DIGEST, "build": {}}}
        b = {"x": {"ref": "r", "digest": "sha256:" + "b" * 64, "build": {}}}
        self.assertEqual(si.merge([("1", a), ("2", a)]), a)
        with self.assertRaisesRegex(si.StackImageError, "twice"):
            si.merge([("1", a), ("2", b)])


if __name__ == "__main__":
    unittest.main()
