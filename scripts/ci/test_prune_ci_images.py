"""Tests for scripts/ci/prune_ci_images.py: what the CI-image cleanup may delete."""

from __future__ import annotations

import datetime as dt
import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import prune_ci_images as p  # noqa: E402

NOW = dt.datetime(2026, 10, 20, tzinfo=dt.timezone.utc)


def v(id_, days_old, tags=()):
    updated = (NOW - dt.timedelta(days=days_old)).isoformat().replace("+00:00", "Z")
    return {"id": id_, "name": f"sha256:{id_:064d}", "updated_at": updated, "metadata": {"container": {"tags": list(tags)}}}


def ids(versions):
    return sorted(x["id"] for x in versions)


class PruneTest(unittest.TestCase):
    def test_non_ci_packages_are_never_selected(self):
        for name in ("elitea-main", "elitea-worker-rust", "buildkit", "charts/elitea", "eci-x", "Ci-x"):
            self.assertEqual(p.select(name, [v(1, 90), v(2, 80)], NOW, 7), [], name)

    def test_image_versions_older_than_the_window_go_tagged_or_not(self):
        versions = [v(1, 30, ["abc"]), v(2, 8), v(3, 6, ["def"]), v(4, 0, ["ghi"])]
        self.assertEqual(ids(p.select("ci-elitea-main", versions, NOW, 7)), [1, 2])

    def test_the_live_cache_tag_is_kept_however_old(self):
        versions = [v(1, 40, ["linux-amd64"]), v(2, 30), v(3, 3)]
        self.assertEqual(ids(p.select("ci-cache-elitea-main", versions, NOW, 7)), [2])

    def test_a_package_is_never_emptied(self):
        versions = [v(1, 30, ["a"]), v(2, 20, ["b"])]
        self.assertEqual(ids(p.select("ci-elitea-web", versions, NOW, 7)), [1])

    def test_nothing_inside_the_window_goes(self):
        self.assertEqual(p.select("ci-elitea-web", [v(1, 6), v(2, 1)], NOW, 7), [])


    def test_package_names_come_from_the_cache_refs(self):
        hcl = 'target "a" {\n  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-main:linux-amd64"]\n}\n'
        hcl += 'target "b" {\n  cache-from = ["type=registry,ref=ghcr.io/elitea-ng/ci-cache-elitea-main:linux-amd64"]\n}\n'
        self.assertEqual(p.ci_packages([hcl]), ["ci-cache-elitea-main", "ci-elitea-main"])
        root = pathlib.Path(__file__).resolve().parents[2]
        names = p.ci_packages([f.read_text() for f in (root / "deploy" / "ci").glob("*.hcl")])
        self.assertIn("ci-elitea-worker-rust", names)
        self.assertTrue(all(n.startswith("ci-") for n in names))


if __name__ == "__main__":
    unittest.main()
