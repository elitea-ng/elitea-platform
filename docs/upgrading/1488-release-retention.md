---
title: "Release retention"
issue: 1488
date: 2026-10-28
order: 36
---

* **Only the newest releases stay in GHCR.** The new **Release Retention**
  workflow (admins only, by hand) keeps the newest 10 stable releases and the
  newest 3 pre-releases of each channel. It deletes every older GitHub
  release with its tag, images, charts and desktop app. publish.yml now keeps
  3 pre-releases per channel instead of 14, and a stable release deletes every
  pre-release cut before it.
* **Retired packages are deleted:** `elitea-ui`, `elitea-worker-python`,
  `elitea-deepwiki`, `elitea-inventory`, the old indexer image, and the
  `charts/nats` and `charts/nats-bootstrap` charts, which the `elitea` chart
  has carried since v4.71.0.
* **What to do:** if a deployment pins an image or chart older than the oldest
  kept release, move it to a kept release before an admin runs the workflow.
  Nothing else changes.
