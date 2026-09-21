"""Deterministic fake Azure DevOps for the standalone stack (wave 4, package W1).

Why this exists
---------------
Nineteen `toolkits-credentials` onetest cases (ELITEA-2590..2596, 2599..2610)
exercise the ADO toolkits' IMAGE pipeline — `ado_boards.get_comments` /
`get_work_item` and `ado_wiki.get_wiki_page*` downloading an attachment and
handing it to a vision model, with an in-memory description cache in front.
Wave 3 ledgered every one of them LIVE-ONLY because there was no ADO-shaped
backend in `deploy/` and no `e2e/live` ADO lane: the cases are about what the
toolkit FETCHED and how often it asked the model, and neither is observable
without a server that can be asked what it served.

This is that server, in the posture of `deploy/qtest-mock/server.py` and
`deploy/mock-llm/server.py`: standard library only, no network at build or run
time, byte-predictable answers, and a journal of every request.

How the SDK actually talks to Azure DevOps
------------------------------------------
NOT by URL. All four toolkits go through the `azure-devops` python client
(7.1.0b4) over `msrest`, and that client addresses every operation by a
LOCATION GUID:

  1. `Connection(base_url, BasicAuthentication('', PAT))` — every request
     carries `Authorization: Basic base64(':' + PAT)`.
  2. Constructing any client calls `GET {base}/_apis/resourceAreas`. An EMPTY
     `value` list means "on-prem": the client then uses `base_url` for every
     service, which is what lets ONE host here serve boards, wiki, git, core
     and test plans at once.
  3. Once per host it calls `OPTIONS {base}/_apis`, whose answer is a catalogue
     of `ApiResourceLocation` rows — `{id, area, resourceName, routeTemplate,
     minVersion, maxVersion, releasedVersion}`. The SERVER chooses the route
     templates; the client formats the template it was given with the route
     values of the call. So `LOCATIONS` below is the wire contract, and the
     paths this file routes on are the ones it advertises.

A location the catalogue omits raises `API resource location <guid> is not
registered`, which is the failure mode to look for when a new tool appears.

What it serves
--------------
  OPTIONS /_apis                                    the location catalogue (see above)
  GET  /_apis/resourceAreas                         empty -> single-host mode
  GET  /_apis/connectionData                        identity probe
  GET  /_apis/projects[/{projectId}]                core
  GET  /{project}/_apis/wit/workItems/{id}          get_work_item
  GET  /{project}/_apis/wit/workItems/{id}/comments get_comments ($expand/$top/order)
  GET  /{project}/_apis/wit/attachments/{id}        the attachment BYTES
  POST /{project}/_apis/wit/attachments             create_attachment
  GET  /{project}/_apis/wit/workItemTypes           check-connection probe
  GET  /{project}/_apis/wiki/wikis[/{id}]           get_all_wikis / get_wiki
  GET  /{project}/_apis/wiki/wikis/{id}/pages       get_page (by ?path=)
  GET  /{project}/_apis/wiki/wikis/{id}/pages/{pid} get_page_by_id
  GET  /{project}/_apis/git/repositories[/{id}]     get_repositories / get_repository
  GET  /{project}/_apis/git/repositories/{id}/items the wiki attachment BYTES
  GET  /{project}/_apis/git/repositories/{id}/stats/branches  branch existence
  GET  /{project}/_apis/testplan/Plans[/{planId}]   plans, suites, test cases

  GET    /__journal  every request, newest last
  DELETE /__journal  empty it
  GET    /__state    the seed as data: attachments (name/md5/bytes), work items,
                     comments, wiki pages, repos, plans — plus `downloads`,
                     the per-attachment download COUNT
  POST   /__reset    back to the seeded state, journal included

Authorisation
-------------
Every `/_apis` route (OPTIONS included) demands the PAT, as
`Authorization: Basic base64(':' + ADO_MOCK_PAT)` — what
`BasicAuthentication('', token)` puts on the wire — or `Bearer <PAT>`, which is
what `AdoConfiguration.check_connection` sends. Anything else is 401. That is
not decoration: half of what these cases assert is that a CREDENTIAL was
resolved and carried, and a backend that answered without one could not tell a
working credential from an absent one.

The seed, and why each row is there
-----------------------------------
Attachments are addressed by GUID, exactly as Azure DevOps addresses them, and
the comment/page markup embeds `.../_apis/wit/attachments/{guid}?fileName=...`
URLs so `_extract_attachment_ref` in the SDK finds them.

  IMG-1 screenshot.png  288x288 PNG   the primary image, repeated on purpose
  IMG-2 diagram.jpg     288x288 JPEG  format coverage (ELITEA-2608)
  IMG-3 flow.gif        288x288 GIF   format coverage
  IMG-4 chart.webp      288x288 WEBP  format coverage
  DOC-1 spec.pdf        a real PDF header, embedded with IMAGE markdown
                        (ELITEA-2590: a non-image must not be described)
  BAD-1 broken.png      text bytes under an image name -> decode failure
  BAD-2 empty.png       zero bytes   -> "could not read image dimensions"
  BAD-3 huge.png        6 MB         -> over the SDK's 5 MB processing cap
  BAD-4 notes.txt       a name with no image extension -> unsupported format
  BAD-5 forbidden.png   answered 403 -> the permission path (ELITEA-2607)
  BAD-6 slow.png        answered after ADO_MOCK_SLOW_SECONDS -> the timeout path
  GEN-1..GEN-12         generated 288x288 solid-colour PNGs, each a distinct
                        digest, for the many-images and dedup cases (2609)

The images are 288x288 for the reason `chat.artifacts-image-read.spec.ts`
records: `EliteAImageLoader` upscales anything under 256px on the long edge,
which re-encodes it and changes the digest. At 288 the loader passes the file
through byte for byte, so the md5 this server publishes in `/__state` is the
md5 the mock LLM's journal reports — which is what makes "the model was shown
THIS attachment" provable rather than plausible.
"""
from __future__ import annotations

import base64
import binascii
import hashlib
import json
import os
import re
import struct
import threading
import time
import zlib
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, unquote, urlparse

PORT = int(os.environ.get("ADO_MOCK_PORT", "8097"))
PAT = os.environ.get("ADO_MOCK_PAT", "e2e-ado-pat")
# What the SEEDED markup uses to address this server. The toolkit reaches the
# mock on the compose network, so the attachment URLs embedded in comments and
# work item fields have to be compose-network URLs too — a published-port URL
# would resolve from the test runner and not from the worker.
PUBLIC_URL = os.environ.get("ADO_MOCK_PUBLIC_URL", f"http://ado-mock:{PORT}").rstrip("/")
PROJECT = os.environ.get("ADO_MOCK_PROJECT", "E2E")
PROJECT_ID = "11111111-2222-3333-4444-555555555555"
WIKI_ID = "e2ewiki"
WIKI_REPO_ID = "aaaaaaaa-0000-4000-8000-000000000001"
CODE_REPO_ID = "aaaaaaaa-0000-4000-8000-000000000002"
# How long `slow.png` takes to answer — the non-blocking-timeout case.
SLOW_SECONDS = float(os.environ.get("ADO_MOCK_SLOW_SECONDS", "8"))

_LOCK = threading.Lock()


# ── The location catalogue the SDK asks for with OPTIONS /_apis ───────────────
#
# (id, area, resourceName, routeTemplate). The GUIDs are Azure DevOps' own and
# are what the generated clients send; the templates are this server's choice,
# shaped like the real ones so a journal entry reads like an ADO access log.
# A `{placeholder}` that is a whole path segment is DROPPED by the client when
# the call supplies no value for it (`_remove_optional_route_parameters`),
# which is how one row serves both `get_comments` and `get_comment`, and how
# `get_attachment_content` reaches `/_apis/wit/attachments/{id}` with no
# project segment at all.
LOCATIONS = (
    ("e81700f7-3be2-46de-8624-2eb35882fcaa", "Location", "ResourceAreas", "_apis/resourceAreas/{areaId}"),
    ("00d9565f-ed9c-4a06-9a50-00e7896ccab4", "Location", "ConnectionData", "_apis/connectionData"),
    ("603fe2ac-9723-48b9-88ad-09305aa6c6e1", "core", "projects", "_apis/projects/{projectId}"),
    ("72c7ddf8-2cdc-4f60-90cd-ab71c14a399b", "wit", "workItems", "{project}/_apis/wit/workItems/{id}"),
    ("608aac0a-32e1-4493-a863-b9cf4566d257", "wit", "comments",
     "{project}/_apis/wit/workItems/{workItemId}/comments/{commentId}"),
    ("e07b5fa4-1499-494d-a496-64b860fd64ff", "wit", "attachments", "{project}/_apis/wit/attachments/{id}"),
    ("7c8d7a76-4a09-43e8-b5df-bd792f4ac6aa", "wit", "workItemTypes", "{project}/_apis/wit/workItemTypes/{type}"),
    ("288d122c-dbd4-451d-aa5f-7dbbba070728", "wiki", "wikis", "{project}/_apis/wiki/wikis/{wikiIdentifier}"),
    ("25d3fbc7-fe3d-46cb-b5a5-0b6f79caf27b", "wiki", "pages",
     "{project}/_apis/wiki/wikis/{wikiIdentifier}/pages"),
    ("ceddcf75-1068-452d-8b13-2d4d76e1f970", "wiki", "pagesById",
     "{project}/_apis/wiki/wikis/{wikiIdentifier}/pages/{id}"),
    ("225f7195-f9c7-4d14-ab28-a83f7ff77e1f", "git", "Repositories",
     "{project}/_apis/git/repositories/{repositoryId}"),
    ("fb93c0db-47ed-4a31-8c20-47552878fb44", "git", "Items",
     "{project}/_apis/git/repositories/{repositoryId}/items"),
    ("d5b216de-d8d5-4d32-ae76-51df755b16d3", "git", "stats/branches",
     "{project}/_apis/git/repositories/{repositoryId}/stats/branches/{name}"),
    ("0e292477-a0c2-47f3-a9b6-34f153d627f4", "testplan", "Plans", "{project}/_apis/testplan/Plans/{planId}"),
    ("1046d5d3-ab61-4ca7-a65a-36118a978256", "testplan", "Suites",
     "{project}/_apis/testplan/Plans/{planId}/Suites/{suiteId}"),
    ("a9bd61ac-45cf-4d13-9441-43dcd01edf8d", "testplan", "TestCase",
     "{project}/_apis/testplan/Plans/{planId}/Suites/{suiteId}/TestCase/{testCaseIds}"),
)


def _locations_payload() -> dict:
    return {
        "count": len(LOCATIONS),
        "value": [
            {
                "id": location_id,
                "area": area,
                "resourceName": resource,
                "routeTemplate": template,
                "resourceVersion": 1,
                # Wide on purpose: the clients in play ask for 7.0 and 7.1, and
                # a window that covers both makes `_negotiate_request_version`
                # hand back the requested version untouched.
                "minVersion": 1.0,
                "maxVersion": 7.2,
                "releasedVersion": "7.2",
            }
            for location_id, area, resource, template in LOCATIONS
        ],
    }


# ── Image bytes ──────────────────────────────────────────────────────────────


def _png(width: int, height: int, rgb: tuple[int, int, int]) -> bytes:
    """A real, minimal, solid-colour PNG — one distinct digest per colour.

    Written here rather than embedded so the many-images and deduplication
    cases can have as many DISTINCT images as they need without pasting a
    kilobyte of base64 per picture.
    """
    row = b"\x00" + bytes(rgb) * width
    raw = row * height

    def chunk(tag: bytes, data: bytes) -> bytes:
        body = tag + data
        return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body) & 0xFFFFFFFF)

    header = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", header)
        + chunk(b"IDAT", zlib.compress(raw, 9))
        + chunk(b"IEND", b"")
    )


# A minimal but structurally real PDF: `%PDF-` magic, one empty page, `%%EOF`.
# Its point is to be plainly NOT an image while wearing image markdown.
PDF_BYTES = (
    b"%PDF-1.4\n"
    b"1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n"
    b"2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n"
    b"3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]>>endobj\n"
    b"trailer<</Root 1 0 R>>\n%%EOF\n"
)


# ── The four embedded pictures ───────────────────────────────────────────────
#
# 288x288 in each format the SDK's `image_loaders_map` lists as directly
# LLM-supported, lifted verbatim from `chat.artifacts-image-read.spec.ts` so
# the two suites describe the same bytes. ABOVE 256px on the long edge
# deliberately: `EliteAImageLoader` upscales anything smaller, which
# re-encodes it as PNG and changes both the format and the digest the model
# journal reports.
PNG_B64 = (
    "iVBORw0KGgoAAAANSUhEUgAAASAAAAEgCAIAAACb4TnXAAADV0lEQVR4nO3VUQ3CABQEQUrQUYwUX0VBhVUJTtAAyeYlzYyC"
    "+9ncsm77DWjcpwfAlQkMQgKDkMAgJDAICQxCAoOQwCAkMAgJDEICg5DAICQwCAkMQgKDkMAgJDAICQxCAoOQwCAkMAgJDEIC"
    "g5DAICQwCAkMQgKDkMAgJDAICQxCAoOQwCAkMAgJDEICg5DAICQwCAkMQgKDkMAgJDAICQxCAoOQwCAkMAgJDEICg5DAICQw"
    "CAkMQgKDkMAgJDAICQxCAoOQwCAkMAgJDEICg9BjesA/PucxPYEZz9d7esJvPBiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQG"
    "IYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQ"
    "EhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYh"
    "gUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBAS"
    "GIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGB"
    "QUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIY"
    "hAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFBSGAQEhiEBAYhgUFIYBASGIQEBiGBQUhgEBIYhAQGIYFB"
    "SGAQWtZtn94Al+XBICQwCAkMQgKDkMAgJDAICQxCAoOQwCAkMAgJDEICg5DAICQwCAkMQgKDkMAgJDAICQxCAoOQwCAkMAgJ"
    "DEICg5DAICQwCAkMQgKDkMAgJDAICQxCAoOQwCAkMAgJDEICg5DAICQwCAkMQgKDkMAgJDAICQxCAoOQwCAkMAgJDEICg5DA"
    "ICQwCAkMQgKDkMAgJDAICQxCAoOQwCAkMAh9AfvbBuVooZbiAAAAAElFTkSuQmCC"
)
JPEG_B64 = (
    "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDABQODxIPDRQSEBIXFRQYHjIhHhwcHj0sLiQySUBMS0dARkVQWnNiUFVtVkVGZIhl"
    "bXd7gYKBTmCNl4x9lnN+gXz/2wBDARUXFx4aHjshITt8U0ZTfHx8fHx8fHx8fHx8fHx8fHx8fHx8fHx8fHx8fHx8fHx8fHx8"
    "fHx8fHx8fHx8fHx8fHz/wAARCAEgASADASIAAhEBAxEB/8QAHwAAAQUBAQEBAQEAAAAAAAAAAAECAwQFBgcICQoL/8QAtRAA"
    "AgEDAwIEAwUFBAQAAAF9AQIDAAQRBRIhMUEGE1FhByJxFDKBkaEII0KxwRVS0fAkM2JyggkKFhcYGRolJicoKSo0NTY3ODk6"
    "Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqDhIWGh4iJipKTlJWWl5iZmqKjpKWmp6ipqrKztLW2t7i5usLDxMXG"
    "x8jJytLT1NXW19jZ2uHi4+Tl5ufo6erx8vP09fb3+Pn6/8QAHwEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoL/8QAtREA"
    "AgECBAQDBAcFBAQAAQJ3AAECAxEEBSExBhJBUQdhcRMiMoEIFEKRobHBCSMzUvAVYnLRChYkNOEl8RcYGRomJygpKjU2Nzg5"
    "OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6goOEhYaHiImKkpOUlZaXmJmaoqOkpaanqKmqsrO0tba3uLm6wsPE"
    "xcbHyMnK0tPU1dbX2Nna4uPk5ebn6Onq8vP09fb3+Pn6/9oADAMBAAIRAxEAPwDKooorlOwKKKKACiiigAooooAKKKKACiii"
    "gAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiii"
    "gAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiii"
    "gAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiim"
    "SypCu5zj0Hc0wH0ySaOL77ge3es+a/d8iMbF9e9VCSSSTknvWih3MnUXQ0H1Ef8ALOMnjqxqBr6djkMF9gP8arUVaikZucmP"
    "M0pBBkcg9txplFFUQFPWWRRhZGA9ATTKKALCXs64+bcB2IqdNR6CSP6lT/SqFFS4plKckbMVxFKBscZPY9alrBqzDfSx8N86"
    "+/X86h0+xqqvc1aKihuI5xlDz6HrUtZ7GqdwooopAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUVSvLvZmOI/"
    "N3YdqpK4m0ldkl1drB8qjc/p6VmO7SNudix96bRW0YpHNKTkFFFFUSFFFFABRRRQAUUUUAFFFFABRRRQAqsVOVJB9RWja3ok"
    "ISThux9azaKlxTKjJxN6is6zu9mI5T8vZj2rRrFqx0RkpIKKKKkoKKKKACiiigAooooAKKKKACiiigAooooAKKKZNKsMZdu3"
    "QeppgQ3lz5ChV++3T2rKp0jmR2dupOabW8Y2RyylzMKKKKokKKKKACiiigAooooAKKKKACiiigAooooAKKKKACtGxud4ET9Q"
    "PlPqKzqASCCDgjvSauioy5Xc3qKhtpxPEG4DD7wHapq52rHUnfUKKKKQBRRRQAUUUUAFFFFABRRRQAUUUUAFZmoTb5fLU/Kn"
    "X61oTSeVE7+g4+tYhJJJJyT3rWC6mVR6WCiiitTAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAns5vJmGT"
    "8rcGtesGte0l823Uk5YcGsqi6m1J9CeiiisjYKKKKACiiigAooooAKKKKACiiigCjqb/AConHJyfWs+rN+xa5IP8IAH8/wCt"
    "Vq6IqyOabvIKKKKogKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKuaa+JWTj5hn8qp1LasVuYyP72Pz4pS"
    "V0VF2Zs0UUVzHUFFFFABRRRQAUUUUAFFFFABRRRQBiTkGeQg5BY8/jTKKK6jjCiiigAooooAKKKKACiiigAooooAKKKKACii"
    "igAooooAKKKKACiiigAooooA3QQQCDkHvS1HB/x7x/7g/lUlcx2IKKKKQBRRRQAUUUUAFFFFABRRRQBg0U+ZQszqvADECmV1"
    "HGFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQBtQf8e8f+4P5VJSKoVQq8ADApa5jsQUUUUgC"
    "iiigAooooAKKKKACiiigDJvl23LcYDYIqvV/U0+44HsT/L+tUK6Iu6OWatIKKKKokKKKKACiiigAooooAKKKKACiiigAoooo"
    "AKKKKACiiigAooooAKkt13zxrjPzDI9qjq3pybpixHCjr6H/ADmk3ZFRV2adFFFcx1BRRRQAUUUUAFFFFABRRRQAUUUUARXE"
    "XmwsuMnGR9axq3qyr+Ly5tw+6/P4961pvoZVV1K1FFFamAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAVq"
    "2MXl24OPmfk/0rPtYvOmCn7o5P0rZrKo+htSXUKKKKyNgooooAKKKKACiiigAooooAKKKKACoriETxFD16j61LRT2Bq5hMpV"
    "ircEHBpK0r62Mg8yMfMOo9aza3i7o5ZR5WFFFFUSFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRV6xtjkSyDj+EH+dJ"
    "uyHFczsWLSDyIvm++33uasUUVzt3OpKysFFFFIYUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAVnXlpszJEPl7qO1aNFUnYmUVJ"
    "GDRWjdWQf5oQFPdegNZxBBIIwR2rdNM55RcdwooopkhRRRQAUUUUAFFFFABRRRQAUUUUAFFFXrWyOQ8w46hf8aTaQ1Fy2I7S"
    "0Mx3vxGP1rTAAAAGAO1AAAAAwB2pawlK50xiooKKKKkoKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAqGe3ScfMM"
    "NjhvSpqKadgavuZE1pLDk43L6ioK3qgltIZSSVwx7rxWiqdzF0uxkUVcfTpB9x1bjvwaga2mU4MbfgM/yrRSTM3Foiooopkh"
    "RRRQAUVIlvK+Nsbc9DjAqdNPlbG8qo79yKTaRSi2VKlhtpZuVXC/3j0rQisoYwMje3q3+FWazdTsaKl3K9vaJBz95/7xqxRR"
    "WbdzVJLYKKKKQwooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigBCAQQRkHtTPIi/55J/3"
    "yKkophYj8iL/AJ5J/wB8inqoUYUAD0FLRQFgooopAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABR"
    "RRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABR"
    "RRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFABRRRQAUUUUAFFFFAH/2Q=="
)
GIF_B64 = (
    "R0lGODdhIAEgAYEAAB54PP8AAAAAAAAAACwAAAAAIAEgAUAI/wADCBxIsCCAgwgTKlzIsKHDhxAjSpxIsaLFixgzatyYsaDH"
    "jwY5ihxJsqTJkyhTcgTJsqVAlTBjypxJs+ZGlzg92tzJs6fPnxdzCn0JtKjRo0hTDhWatKnTp1AZLs0ZtarVqzyn4sTKtavX"
    "kVpdfh1LtqzDsC3Nql37FS1LtnDjQnULUq7du0DpfsTLt+9MvTr9Ch5MEnBIwogTVzRMULHjx2cZE4VMGbLkyZUzI74cQLNn"
    "wpw/i+4berRpuaVPq1aberXrtpdfy/baerbtubFv635ae7fvvLl/C//Ze7jxv8GPK5dZfLnzks2fS7+ZfLp16pKva8fOeLv3"
    "oJzFfv8fTzE7+fORzaNfnzA6++/u32+PL/86/frT7+N/rn//8v7+HQdggMMNSOBvBh64W4IK3sZgg7M9COFrEk64WoUWnoZh"
    "hqNtyOFnHn6oWYgiVkZiiZZVhyKCKq64YIsuOghjjBHOSCOFNt54YY46ashjjx3+CCSIQg45YpFGmmhYkrqFdxiTNYYFJY56"
    "TWnaiVaahWWWZG3JJWzqfalkmGKmSGaZjnmJ5lVqrllVm27idmacg8FJZ1N23olUnnoaxWefwM0J6F1/DtpToYbuhGiiNS3K"
    "KHKCPrqWo5LCRGmlSiGJKZuabvpmp57K2V2obF1KKlignpqUqapyt2SrZbH/CitGss5qUa22lpdqrj7hymtEvv76ULDCNkRs"
    "sQsdi2x7uy4L6ajOivpqtLw1S61KykabrbPbLtstst8WG66w4/5aLq/n5pqurevO2i6s77Yar6rznlovqfeGmq+n+27aL6b/"
    "VhqwpNZemyldBluFVsJcOdkZw5wuBfGnWk1c7cIW71llxkVBy3FWkX5sacEi30pyybqGjPJJAyfasqEvDxozoDP3WbOeN9+Z"
    "M507x9mzmz+vGTSaQ5dZtJhHf5k0l0tn2bSVT08ZNZRTM1l1klcbmfWQWwPZdY9f6xj2jWPTWHaMZ7uY9opro9h2iW+LGPeH"
    "c3NYd4Z3W5j3hHtD/9h3g38rGPiBgxNYeICH+5f4fovj13h9j8sX+XuTs1f5epejl/l5m5PX+Xifw3fyysCOTvqwpp9ubOqq"
    "J8t668yqDLurgM2O7eu2h+6d7vPhPjvv2gFvn++wC2+d8fkR3zry0jHPn/KqO++c9P9Bfzr1ymEvoPWka2+c9wVyvzL4wpHP"
    "ouy20yo+yub71v6L6Kdvcvzyp+xx/SK93+T6JesvI/34Kx0AA4i6ARJwdQY8oOsSqMDY3a+B83sgBO03rQlGsIIWpGDtMnjB"
    "DXJQgxv74ET8ZxsSRkmCIkQgClO4wBWy0IEYfKFU+CcyE8rGhlRyoQwBgEPX9HBHDJzgDyBVM0QfxXCHLXQLEiUSwiVCRIlO"
    "HGHFogjCtFBxMUIJCAA7"
)
WEBP_B64 = (
    "UklGRmgBAABXRUJQVlA4IFwBAACQGgCdASogASABPm02mEikI6KhJHd4AIANiWdu4XdhGl1gBphFYFrhAKqLtkjmsC1wgFVF"
    "2yRzWBa4QCqf0r4Nc1d0XLsPvS1KpmE74BU/6CDCqi6GtLSRbW5dkjmqRTtwfjW9ALXB98MwqoB0tQQCqfwZF2xVYeyRzV84"
    "C4QBTpaggFU/gyLtiqw9kjmr5wFwgCnS1BAKp/BkXbFVh7JHNXzgLhAFOlqCAVT+DIu2J4jwAsCwLAsCwLAsCwLAjvGSyOVq"
    "2SOawLXKdVRdskc1gWuEAqou2SOawLW7gAD+/4qd//+Ur/8bv+wXb7879zMAAADR/wZT+6vtU73dXwojiidiN8P7OP7w0pj/"
    "1ToL/WhI/GP0sGJJTL7SxZBk2rZaCQewoAAAAAAVfYN+qOxAeCcb/q8EY7p3caR7vbWvuawpPxApQGcBlzAnR+/UHAe++Pnf"
    "kjN+6BAAAAA="
)


def _guid(tag: str) -> str:
    """A stable, readable attachment GUID — `_extract_attachment_ref` accepts
    any `[\\w-]+` segment, and a name in the id makes a journal entry legible."""
    digest = hashlib.md5(tag.encode()).hexdigest()
    return f"{digest[:8]}-{digest[8:12]}-4{digest[13:16]}-8{digest[17:20]}-{digest[20:32]}"


# id -> the attachment record. `status` is what the download route answers with
# instead of the bytes, and is what gives 2601/2607 their 403 and timeout
# without any network trickery.
def _attachments() -> dict:
    generated = {
        f"gen-{index:02d}": {
            "name": f"generated-{index:02d}.png",
            "bytes": _png(288, 288, (17 * index % 256, 89 * index % 256, 211 * index % 256)),
        }
        for index in range(1, 13)
    }
    catalogue = {
        "img-1": {"name": "screenshot.png", "bytes": base64.b64decode(PNG_B64)},
        "img-2": {"name": "diagram.jpg", "bytes": base64.b64decode(JPEG_B64)},
        "img-3": {"name": "flow.gif", "bytes": base64.b64decode(GIF_B64)},
        "img-4": {"name": "chart.webp", "bytes": base64.b64decode(WEBP_B64)},
        "doc-1": {"name": "spec.pdf", "bytes": PDF_BYTES},
        "bad-1": {"name": "broken.png", "bytes": b"this is not a PNG, whatever the name says\n" * 8},
        "bad-2": {"name": "empty.png", "bytes": b""},
        # Over the SDK's MAX_IMAGE_READ_BYTES (5 MB). Incompressible on purpose:
        # a zlib-friendly filler would make the PNG itself small.
        "bad-3": {"name": "huge.png", "bytes": os.urandom(6 * 1024 * 1024)},
        "bad-4": {"name": "notes.txt", "bytes": b"plain text, no image extension\n"},
        "bad-5": {"name": "forbidden.png", "bytes": base64.b64decode(PNG_B64), "status": 403},
        "bad-6": {"name": "slow.png", "bytes": base64.b64decode(PNG_B64), "delay": SLOW_SECONDS},
        **generated,
    }
    out = {}
    for tag, record in catalogue.items():
        out[_guid(tag)] = {
            "tag": tag,
            "id": _guid(tag),
            "name": record["name"],
            "bytes": record["bytes"],
            "md5": hashlib.md5(record["bytes"]).hexdigest(),
            "size": len(record["bytes"]),
            "status": record.get("status", 200),
            "delay": record.get("delay", 0.0),
        }
    return out


ATTACHMENTS = _attachments()
BY_TAG = {record["tag"]: record for record in ATTACHMENTS.values()}


def attachment_url(tag: str) -> str:
    record = BY_TAG[tag]
    return f"{PUBLIC_URL}/{PROJECT}/_apis/wit/attachments/{record['id']}?fileName={record['name']}"


def _img(tag: str) -> str:
    """An HTML `<img>` exactly as Azure DevOps renders an inline attachment."""
    return f'<img src="{attachment_url(tag)}" alt="{BY_TAG[tag]["name"]}">'


def _md(tag: str) -> str:
    """Image MARKDOWN — the form ADO Wiki uses for EVERY attachment type."""
    return f"![{BY_TAG[tag]['name']}]({attachment_url(tag)})"


def _comment(comment_id: int, work_item_id: int, html: str) -> dict:
    return {
        "id": comment_id,
        "workItemId": work_item_id,
        "version": 1,
        "text": html,
        "renderedText": html,
        "format": "html",
        "createdBy": {"displayName": "Autotest", "uniqueName": "autotest@example.com"},
        "createdDate": "2026-01-01T00:00:00Z",
        "modifiedDate": "2026-01-01T00:00:00Z",
        "url": f"{PUBLIC_URL}/{PROJECT}/_apis/wit/workItems/{work_item_id}/comments/{comment_id}",
    }


# ── The seed ─────────────────────────────────────────────────────────────────
#
# Each work item exists for a named group of cases; the comment bodies ARE the
# fixture, so changing one changes what a case can assert.
def _work_items() -> dict:
    def item(work_item_id: int, title: str, description: str, relations: list) -> dict:
        return {
            "id": work_item_id,
            "rev": 1,
            "fields": {
                "System.Id": work_item_id,
                "System.WorkItemType": "Bug",
                "System.Title": title,
                "System.State": "Active",
                "System.Description": description,
            },
            "relations": relations,
            "url": f"{PUBLIC_URL}/{PROJECT}/_apis/wit/workItems/{work_item_id}",
        }

    def attached(tag: str) -> dict:
        record = BY_TAG[tag]
        return {
            "rel": "AttachedFile",
            "url": attachment_url(tag),
            "attributes": {"name": record["name"], "resourceSize": record["size"]},
        }

    return {
        # 101 — ONE image, reached from three comments AND from the work item's
        # own attachment relation. The cache claims (2592, 2593, 2594, 2596,
        # 2603) all read off this item: every occurrence is the same GUID, so
        # the same bytes, so the same cache key.
        101: item(
            101,
            "Autotest repeated image",
            f"<div>The failure is visible here: {_img('img-1')}</div>",
            [attached("img-1")],
        ),
        # 102 — one image per supported format, one per comment (2608, 2604).
        102: item(102, "Autotest image formats", "<div>Formats under test.</div>", [attached("img-2")]),
        # 103 — DETECTION: html img, image markdown, a non-image attachment in
        # image markdown, a malformed URL and an absent attachment (2600, 2607).
        103: item(103, "Autotest mixed content", "<div>Mixed.</div>", []),
        # 104 — every download/validation failure the SDK has a branch for
        # (2601, 2607): corrupt bytes, zero bytes, over the 5 MB cap, 403, slow.
        104: item(104, "Autotest broken attachments", "<div>Broken.</div>", []),
        # 105 — twelve DISTINCT images plus a repeat of the first, in ONE
        # comment (2609: many images, and dedup inside a single body).
        105: item(105, "Autotest many images", "<div>Many.</div>", []),
        # 106 — no image anywhere. The graceful-skip baseline (2600 step 5).
        106: item(106, "Autotest text only", "<div>No pictures at all.</div>", []),
    }


def _comments() -> dict:
    absent = _guid("absent-attachment")
    return {
        101: [
            _comment(1, 101, f"<p>first sighting</p>{_img('img-1')}"),
            _comment(2, 101, "<p>a comment with no image at all</p>"),
            _comment(3, 101, f"<p>same picture again</p>{_img('img-1')}"),
            _comment(4, 101, "<p>still no image</p>"),
            _comment(5, 101, f"<p>and once more</p>{_img('img-1')}"),
        ],
        102: [
            _comment(1, 102, f"<p>png</p>{_img('img-1')}"),
            _comment(2, 102, f"<p>jpeg</p>{_img('img-2')}"),
            _comment(3, 102, f"<p>gif</p>{_img('img-3')}"),
            _comment(4, 102, f"<p>webp</p>{_img('img-4')}"),
        ],
        103: [
            _comment(1, 103, f"<p>html tag</p>{_img('img-1')}"),
            _comment(2, 103, f"markdown image {_md('img-3')} in a text comment"),
            _comment(3, 103, f"a document wearing image markdown {_md('doc-1')}"),
            _comment(4, 103, f"<p>not an image by name</p>{_img('bad-4')}"),
            _comment(5, 103, '<p>malformed</p><img src="http://[bad]/nope.png">'),
            _comment(
                6,
                103,
                f'<p>gone</p><img src="{PUBLIC_URL}/{PROJECT}/_apis/wit/attachments/{absent}?fileName=gone.png">',
            ),
            _comment(7, 103, "<p>plain text only</p>"),
        ],
        104: [
            _comment(1, 104, f"<p>corrupt</p>{_img('bad-1')}"),
            _comment(2, 104, f"<p>empty</p>{_img('bad-2')}"),
            _comment(3, 104, f"<p>oversized</p>{_img('bad-3')}"),
            _comment(4, 104, f"<p>forbidden</p>{_img('bad-5')}"),
            _comment(5, 104, f"<p>and one that works</p>{_img('img-1')}"),
        ],
        105: [
            _comment(
                1,
                105,
                "<p>a wall of pictures</p>"
                + "".join(_img(f"gen-{index:02d}") for index in range(1, 13))
                + _img("gen-01"),
            ),
        ],
        106: [
            _comment(1, 106, "<p>nothing to look at</p>"),
            _comment(2, 106, "<p>nothing here either</p>"),
        ],
    }


# Wiki pages. ADO Wiki writes EVERY attachment as image markdown pointing at
# `/.attachments/<name>`, and the SDK resolves those through the wiki's git
# repository — which is why `_git_items` below carries the same bytes under
# `.attachments/`.
def _wiki_pages() -> dict:
    return {
        "/Images": (
            "# Images\n\nA real picture:\n\n"
            "![screenshot.png](/.attachments/screenshot.png)\n\n"
            "And a document, in the same markdown:\n\n"
            "![spec.pdf](/.attachments/spec.pdf)\n\nEnd of page.\n"
        ),
        "/Repeat": (
            "# Repeat\n\nOnce:\n\n![screenshot.png](/.attachments/screenshot.png)\n\n"
            "Twice:\n\n![again.png](/.attachments/screenshot.png)\n"
        ),
        "/Formats": (
            "# Formats\n\n![diagram.jpg](/.attachments/diagram.jpg)\n\n"
            "![flow.gif](/.attachments/flow.gif)\n\n![chart.webp](/.attachments/chart.webp)\n"
        ),
        "/Broken": (
            "# Broken\n\n![broken.png](/.attachments/broken.png)\n\n"
            "![missing.png](/.attachments/missing.png)\n\n"
            "![screenshot.png](/.attachments/screenshot.png)\n"
        ),
        "/Plain": "# Plain\n\nNo pictures on this page.\n",
    }


WIKI_PAGE_IDS = {path: 900 + index for index, path in enumerate(sorted(_wiki_pages()), start=1)}


def _git_items() -> dict:
    """path -> bytes, per repository. The wiki repo's `.attachments/` is what
    `ReposApiWrapper.download_file` reads for every wiki image."""
    wiki_files = {
        ".attachments/screenshot.png": BY_TAG["img-1"]["bytes"],
        ".attachments/diagram.jpg": BY_TAG["img-2"]["bytes"],
        ".attachments/flow.gif": BY_TAG["img-3"]["bytes"],
        ".attachments/chart.webp": BY_TAG["img-4"]["bytes"],
        ".attachments/spec.pdf": BY_TAG["doc-1"]["bytes"],
        ".attachments/broken.png": BY_TAG["bad-1"]["bytes"],
    }
    code_files = {
        "README.md": b"# E2E repository\n\nSeeded by deploy/ado-mock.\n",
        "docs/screenshot.png": BY_TAG["img-1"]["bytes"],
    }
    return {WIKI_REPO_ID: wiki_files, CODE_REPO_ID: code_files}


def _seed() -> dict:
    return {
        "work_items": _work_items(),
        "comments": _comments(),
        "wiki_pages": dict(_wiki_pages()),
        "git_items": _git_items(),
        "plans": {
            501: {
                "id": 501,
                "name": "Autotest plan",
                "areaPath": PROJECT,
                "iteration": PROJECT,
                "project": {"id": PROJECT_ID, "name": PROJECT},
                "rootSuite": {"id": 601, "name": "Autotest plan"},
            }
        },
        "suites": {
            601: {
                "id": 601,
                "name": "Autotest suite",
                "suiteType": "staticTestSuite",
                "plan": {"id": 501, "name": "Autotest plan"},
                "project": {"id": PROJECT_ID, "name": PROJECT},
            }
        },
        "test_cases": {
            601: [
                {
                    "testPlan": {"id": 501},
                    "testSuite": {"id": 601},
                    "workItem": {
                        "id": 701,
                        "name": "Autotest case one",
                        "workItemFields": [{"System.Title": "Autotest case one"}],
                    },
                },
                {
                    "testPlan": {"id": 501},
                    "testSuite": {"id": 601},
                    "workItem": {
                        "id": 702,
                        "name": "Autotest case two",
                        "workItemFields": [{"System.Title": "Autotest case two"}],
                    },
                },
            ]
        },
        # attachment id / git path -> how many times its BYTES were served.
        # The discriminator these cases turn on: the SDK re-downloads on every
        # occurrence and only the MODEL call is cached, so three downloads and
        # one vision request is the passing shape, not a contradiction.
        "downloads": {},
        "uploads": [],
        "next_attachment": 1,
    }


STATE = _seed()
JOURNAL: list[dict] = []


def _now() -> str:
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat()


def _count_download(key: str) -> int:
    with _LOCK:
        total = STATE["downloads"].get(key, 0) + 1
        STATE["downloads"][key] = total
        return total


_PROJECT_SEGMENT = r"(?:[^/]+/)?"


def _route(pattern: str) -> re.Pattern:
    """A route regex with an OPTIONAL leading project segment.

    Not cosmetic: `get_attachment_content` is called with no project at all
    (`_get_attachment_content_capped` passes only the id), so the client drops
    the `{project}` segment from the template and the same operation arrives at
    two different paths.
    """
    return re.compile("^/" + _PROJECT_SEGMENT + pattern + "$")


R_WORK_ITEM = _route(r"_apis/wit/workItems/(\d+)")
R_COMMENTS = _route(r"_apis/wit/workItems/(\d+)/comments")
R_ATTACHMENT = _route(r"_apis/wit/attachments/([\w-]+)")
R_ATTACHMENTS = _route(r"_apis/wit/attachments")
R_WORK_ITEM_TYPES = _route(r"_apis/wit/workItemTypes")
R_WIKIS = _route(r"_apis/wiki/wikis")
R_WIKI = _route(r"_apis/wiki/wikis/([^/]+)")
R_WIKI_PAGES = _route(r"_apis/wiki/wikis/([^/]+)/pages")
R_WIKI_PAGE_BY_ID = _route(r"_apis/wiki/wikis/([^/]+)/pages/(\d+)")
R_REPOS = _route(r"_apis/git/repositories")
R_REPO = _route(r"_apis/git/repositories/([^/]+)")
R_GIT_ITEMS = _route(r"_apis/git/repositories/([^/]+)/items")
R_GIT_BRANCH = _route(r"_apis/git/repositories/([^/]+)/stats/branches")
R_PLANS = _route(r"_apis/testplan/Plans")
R_PLAN = _route(r"_apis/testplan/Plans/(\d+)")
R_SUITES = _route(r"_apis/testplan/Plans/(\d+)/Suites")
R_SUITE = _route(r"_apis/testplan/Plans/(\d+)/Suites/(\d+)")
R_TEST_CASES = _route(r"_apis/testplan/Plans/(\d+)/Suites/(\d+)/TestCase")
R_PROJECTS = re.compile(r"^/_apis/projects$")
R_PROJECT = re.compile(r"^/_apis/projects/([^/]+)$")

# The branches every repository here has. `ReposApiWrapper` refuses to build
# unless one of base_branch/active_branch exists, and the wiki wrapper always
# asks for `wikiMaster`.
BRANCHES = ("wikiMaster", "main")


def _wiki_page_payload(path: str, include_content: bool) -> dict | None:
    content = STATE["wiki_pages"].get(path)
    if content is None:
        return None
    payload = {
        "id": WIKI_PAGE_IDS[path],
        "path": path,
        "gitItemPath": f"/{path.lstrip('/')}.md",
        "order": 0,
        "isParentPage": False,
        "isNonConformant": False,
        "remoteUrl": f"{PUBLIC_URL}/{PROJECT}/_wiki/wikis/{WIKI_ID}{path}",
        "url": f"{PUBLIC_URL}/{PROJECT}/_apis/wiki/wikis/{WIKI_ID}/pages/{WIKI_PAGE_IDS[path]}",
        "subPages": [],
    }
    if include_content:
        payload["content"] = content
    return payload


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    # ── plumbing ────────────────────────────────────────────────────────────
    def log_message(self, fmt: str, *args) -> None:  # noqa: A002 - stdlib name
        print(f"ado-mock {self.address_string()} {fmt % args}", flush=True)

    def _body(self) -> bytes:
        length = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(length) if length else b""

    def _send_json(self, status: int, payload: object) -> None:
        self._send_raw(status, json.dumps(payload).encode("utf-8"), "application/json; charset=utf-8")

    def _send_raw(self, status: int, data: bytes, content_type: str) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(data)

    def _authorised(self) -> bool:
        """`BasicAuthentication('', PAT)` puts `Basic base64(':'+PAT)` on the
        wire; `AdoConfiguration.check_connection` sends `Bearer PAT`. Both are
        the same credential, so both are accepted — and nothing else is."""
        header = self.headers.get("Authorization", "")
        if header.startswith("Bearer "):
            return header[len("Bearer "):].strip() == PAT
        if not header.startswith("Basic "):
            return False
        try:
            decoded = base64.b64decode(header[len("Basic "):].strip()).decode("utf-8", "replace")
        except (ValueError, binascii.Error):
            return False
        user, _, password = decoded.partition(":")
        return PAT in (password, user)

    def _record(self, path: str, query: str, status: int, auth_ok: bool, extra: dict | None = None) -> None:
        entry = {
            "at": _now(),
            "method": self.command,
            "path": path,
            "query": query,
            "status": status,
            "auth_ok": auth_ok,
        }
        if extra:
            entry.update(extra)
        with _LOCK:
            JOURNAL.append(entry)

    # ── control surface ─────────────────────────────────────────────────────
    def _control(self, path: str) -> bool:
        if path == "/healthz":
            self._send_json(200, {"status": "ok"})
            return True
        if path == "/__journal" and self.command == "GET":
            with _LOCK:
                self._send_json(200, list(JOURNAL))
            return True
        if path == "/__journal" and self.command == "DELETE":
            with _LOCK:
                JOURNAL.clear()
            self._send_json(200, {"status": "cleared"})
            return True
        if path == "/__state" and self.command == "GET":
            with _LOCK:
                self._send_json(200, self._state_payload())
            return True
        if path == "/__reset" and self.command == "POST":
            with _LOCK:
                STATE.clear()
                STATE.update(_seed())
                JOURNAL.clear()
                # Attachments UPLOADED during a test are not part of the seed,
                # so a reset that left them behind would make `/__state` say
                # the backend holds rows the seed never described.
                for uploaded in [tag for tag in BY_TAG if tag.startswith("upload-")]:
                    ATTACHMENTS.pop(BY_TAG.pop(uploaded)["id"], None)
            self._send_json(200, {"status": "reset"})
            return True
        return False

    @staticmethod
    def _state_payload() -> dict:
        """The seed as DATA — never the image bytes themselves.

        The digest, the size and the name are what a test needs (`the model was
        shown THIS attachment`), and shipping six megabytes of `huge.png`
        through every `/__state` read would make the reads themselves the
        slowest thing in the suite.
        """
        return {
            "project": {"id": PROJECT_ID, "name": PROJECT},
            "wiki": {"id": WIKI_ID, "repositoryId": WIKI_REPO_ID},
            "attachments": {
                record["tag"]: {
                    "id": record["id"],
                    "name": record["name"],
                    "md5": record["md5"],
                    "size": record["size"],
                    "status": record["status"],
                    "url": attachment_url(record["tag"]),
                }
                for record in ATTACHMENTS.values()
            },
            "work_items": {
                str(work_item_id): item["fields"]["System.Title"]
                for work_item_id, item in STATE["work_items"].items()
            },
            "comments": {
                str(work_item_id): [comment["id"] for comment in comments]
                for work_item_id, comments in STATE["comments"].items()
            },
            "wiki_pages": sorted(STATE["wiki_pages"]),
            "git_items": {
                repository: sorted(files) for repository, files in STATE["git_items"].items()
            },
            "plans": sorted(STATE["plans"]),
            "downloads": dict(STATE["downloads"]),
            "uploads": list(STATE["uploads"]),
        }

    # ── routing ─────────────────────────────────────────────────────────────
    def do_OPTIONS(self) -> None:  # noqa: N802 - stdlib name
        parsed = urlparse(self.path)
        if parsed.path.rstrip("/").endswith("/_apis") or parsed.path.rstrip("/") == "/_apis":
            auth_ok = self._authorised()
            status = 200 if auth_ok else 401
            self._record(parsed.path, parsed.query, status, auth_ok)
            if not auth_ok:
                self._send_json(401, {"message": "unauthorised"})
                return
            self._send_json(200, _locations_payload())
            return
        self._send_json(404, {"message": f"no route for OPTIONS {parsed.path}"})

    def do_HEAD(self) -> None:  # noqa: N802 - stdlib name
        self.do_GET()

    def do_GET(self) -> None:  # noqa: N802 - stdlib name
        parsed = urlparse(self.path)
        path = parsed.path
        if self._control(path):
            return
        if "/_apis" not in path:
            self._send_json(404, {"message": f"no route for {path}"})
            return
        auth_ok = self._authorised()
        if not auth_ok:
            self._record(path, parsed.query, 401, False)
            self._send_json(401, {"message": "unauthorised - the PAT is missing or wrong"})
            return
        query = parse_qs(parsed.query)
        try:
            status, payload, raw, content_type, extra = self._route_get(path, query)
        except Exception as error:  # noqa: BLE001 - a mock must answer, not crash
            self._record(path, parsed.query, 500, True, {"error": str(error)})
            self._send_json(500, {"message": str(error)})
            return
        self._record(path, parsed.query, status, True, extra)
        if raw is not None:
            self._send_raw(status, raw, content_type)
            return
        self._send_json(status, payload)

    def _route_get(self, path: str, query: dict):
        """-> (status, json payload, raw bytes, content type, journal extras)."""
        if path == "/_apis/resourceAreas":
            # EMPTY on purpose: an empty list is the on-prem answer, and it is
            # what makes the client use base_url for every service instead of
            # looking for per-area hosts that do not exist here.
            return 200, {"count": 0, "value": []}, None, "", None
        if path == "/_apis/connectionData":
            return 200, {
                "authenticatedUser": {"id": PROJECT_ID, "providerDisplayName": "Autotest"},
                "authorizedUser": {"id": PROJECT_ID, "providerDisplayName": "Autotest"},
                "instanceId": PROJECT_ID,
                "deploymentType": "onPremises",
            }, None, "", None

        if R_PROJECTS.match(path):
            return 200, {"count": 1, "value": [self._project()]}, None, "", None
        match = R_PROJECT.match(path)
        if match:
            return 200, self._project(), None, "", None

        match = R_ATTACHMENT.match(path)
        if match:
            return self._attachment(match.group(1))

        match = R_COMMENTS.match(path)
        if match:
            return self._comments(int(match.group(1)), query)

        match = R_WORK_ITEM.match(path)
        if match:
            return self._work_item(int(match.group(1)), query)

        if R_WORK_ITEM_TYPES.match(path):
            return 200, {"count": 1, "value": [{"name": "Bug", "referenceName": "Microsoft.VSTS.WorkItemTypes.Bug"}]}, None, "", None

        match = R_WIKI_PAGE_BY_ID.match(path)
        if match:
            return self._wiki_page_by_id(int(match.group(2)), query)
        match = R_WIKI_PAGES.match(path)
        if match:
            return self._wiki_page(query)
        match = R_WIKI.match(path)
        if match:
            return self._wiki(match.group(1))
        if R_WIKIS.match(path):
            return 200, {"count": 1, "value": [self._wiki_payload()]}, None, "", None

        match = R_GIT_ITEMS.match(path)
        if match:
            return self._git_item(match.group(1), query)
        match = R_GIT_BRANCH.match(path)
        if match:
            return self._git_branch(query)
        match = R_REPO.match(path)
        if match:
            return self._repository(match.group(1))
        if R_REPOS.match(path):
            return 200, {
                "count": 2,
                "value": [self._repository(WIKI_REPO_ID)[1], self._repository(CODE_REPO_ID)[1]],
            }, None, "", None

        match = R_TEST_CASES.match(path)
        if match:
            cases = STATE["test_cases"].get(int(match.group(2)), [])
            return 200, {"count": len(cases), "value": cases}, None, "", None
        match = R_SUITE.match(path)
        if match:
            suite = STATE["suites"].get(int(match.group(2)))
            if suite is None:
                return 404, {"message": "suite not found"}, None, "", None
            return 200, suite, None, "", None
        match = R_SUITES.match(path)
        if match:
            suites = [suite for suite in STATE["suites"].values() if suite["plan"]["id"] == int(match.group(1))]
            return 200, {"count": len(suites), "value": suites}, None, "", None
        match = R_PLAN.match(path)
        if match:
            plan = STATE["plans"].get(int(match.group(1)))
            if plan is None:
                return 404, {"message": "plan not found"}, None, "", None
            return 200, plan, None, "", None
        if R_PLANS.match(path):
            plans = list(STATE["plans"].values())
            return 200, {"count": len(plans), "value": plans}, None, "", None

        return 404, {"message": f"no route for {path}"}, None, "", None

    @staticmethod
    def _project() -> dict:
        return {
            "id": PROJECT_ID,
            "name": PROJECT,
            "state": "wellFormed",
            "visibility": "private",
            "url": f"{PUBLIC_URL}/_apis/projects/{PROJECT_ID}",
        }

    @staticmethod
    def _wiki_payload() -> dict:
        return {
            "id": WIKI_ID,
            "name": WIKI_ID,
            "type": "projectWiki",
            "projectId": PROJECT_ID,
            "repositoryId": WIKI_REPO_ID,
            "mappedPath": "/",
            "remoteUrl": f"{PUBLIC_URL}/{PROJECT}/_wiki/wikis/{WIKI_ID}",
            "url": f"{PUBLIC_URL}/{PROJECT}/_apis/wiki/wikis/{WIKI_ID}",
            "versions": [{"version": "wikiMaster", "versionType": "branch"}],
        }

    def _wiki(self, identifier: str):
        if identifier not in (WIKI_ID, WIKI_REPO_ID):
            return 404, {"message": f"wiki '{identifier}' not found"}, None, "", None
        return 200, self._wiki_payload(), None, "", None

    def _wiki_page(self, query: dict):
        path = (query.get("path") or ["/"])[0]
        include = (query.get("includeContent") or ["false"])[0].lower() == "true"
        payload = _wiki_page_payload(path, include)
        if payload is None:
            return 404, {"message": f"wiki page '{path}' not found"}, None, "", None
        return 200, payload, None, "", None

    def _wiki_page_by_id(self, page_id: int, query: dict):
        include = (query.get("includeContent") or ["false"])[0].lower() == "true"
        for path, known_id in WIKI_PAGE_IDS.items():
            if known_id == page_id:
                return 200, _wiki_page_payload(path, include), None, "", None
        return 404, {"message": f"wiki page {page_id} not found"}, None, "", None

    @staticmethod
    def _repository(repository_id: str):
        if repository_id not in (WIKI_REPO_ID, CODE_REPO_ID):
            return 404, {"message": f"repository '{repository_id}' not found"}, None, "", None
        name = "e2e-wiki" if repository_id == WIKI_REPO_ID else "e2e-repo"
        return 200, {
            "id": repository_id,
            "name": name,
            "project": {"id": PROJECT_ID, "name": PROJECT},
            "defaultBranch": "refs/heads/wikiMaster" if repository_id == WIKI_REPO_ID else "refs/heads/main",
            "url": f"{PUBLIC_URL}/{PROJECT}/_apis/git/repositories/{repository_id}",
            "remoteUrl": f"{PUBLIC_URL}/{PROJECT}/_git/{name}",
            "size": 1024,
        }, None, "", None

    @staticmethod
    def _git_branch(query: dict):
        name = (query.get("name") or [""])[0]
        if name and name not in BRANCHES:
            return 404, {"message": f"branch '{name}' not found"}, None, "", None
        if name:
            return 200, {"name": name, "aheadCount": 0, "behindCount": 0,
                         "commit": {"commitId": "0" * 40}}, None, "", None
        return 200, {"count": len(BRANCHES),
                     "value": [{"name": branch, "commit": {"commitId": "0" * 40}} for branch in BRANCHES]}, None, "", None

    def _git_item(self, repository_id: str, query: dict):
        files = STATE["git_items"].get(repository_id)
        if files is None:
            return 404, {"message": f"repository '{repository_id}' not found"}, None, "", None
        item_path = unquote((query.get("path") or [""])[0]).lstrip("/")
        if item_path not in files:
            return 404, {"message": f"item '{item_path}' not found"}, None, "", None
        content = files[item_path]
        total = _count_download(f"git:{repository_id}:{item_path}")
        return 200, None, content, "application/octet-stream", {
            "git_path": item_path,
            "bytes": len(content),
            "download_count": total,
        }

    def _attachment(self, attachment_id: str):
        record = ATTACHMENTS.get(attachment_id)
        if record is None:
            return 404, {"message": f"attachment '{attachment_id}' does not exist"}, None, "", None
        if record["delay"]:
            time.sleep(record["delay"])
        total = _count_download(record["tag"])
        extra = {
            "attachment": record["tag"],
            "file_name": record["name"],
            "bytes": record["size"],
            "download_count": total,
        }
        if record["status"] != 200:
            return record["status"], {
                "message": f"access to attachment '{record['name']}' is denied",
            }, None, "", extra
        return 200, None, record["bytes"], "application/octet-stream", extra

    def _work_item(self, work_item_id: int, query: dict):
        item = STATE["work_items"].get(work_item_id)
        if item is None:
            return 404, {"message": f"work item {work_item_id} does not exist"}, None, "", None
        expand = (query.get("$expand") or [""])[0].lower()
        payload = {"id": item["id"], "rev": item["rev"], "fields": dict(item["fields"]), "url": item["url"]}
        if expand in ("relations", "all"):
            payload["relations"] = [dict(relation) for relation in item["relations"]]
        fields = (query.get("fields") or [""])[0]
        if fields:
            wanted = [name.strip() for name in fields.split(",") if name.strip()]
            payload["fields"] = {name: item["fields"].get(name) for name in wanted}
        return 200, payload, None, "", None

    def _comments(self, work_item_id: int, query: dict):
        comments = STATE["comments"].get(work_item_id)
        if comments is None:
            return 404, {"message": f"work item {work_item_id} does not exist"}, None, "", None
        expand = (query.get("$expand") or ["none"])[0].lower()
        top = int((query.get("$top") or ["200"])[0])
        order = (query.get("order") or ["asc"])[0].lower()
        ordered = list(reversed(comments)) if order == "desc" else list(comments)
        window = ordered[:top]
        rendered = []
        for comment in window:
            row = dict(comment)
            if expand not in ("renderedtext", "all"):
                # The real API omits `renderedText` unless it was asked for,
                # which is exactly why `get_comments` forces the expand when
                # `process_images` is on.
                row.pop("renderedText", None)
            rendered.append(row)
        return 200, {
            "totalCount": len(comments),
            "count": len(rendered),
            "comments": rendered,
            "url": f"{PUBLIC_URL}/{PROJECT}/_apis/wit/workItems/{work_item_id}/comments",
        }, None, "", {"expand": expand, "comments": len(rendered)}

    def do_POST(self) -> None:  # noqa: N802 - stdlib name
        parsed = urlparse(self.path)
        path = parsed.path
        if self._control(path):
            return
        body = self._body()
        if "/_apis" not in path:
            self._send_json(404, {"message": f"no route for {path}"})
            return
        auth_ok = self._authorised()
        if not auth_ok:
            self._record(path, parsed.query, 401, False)
            self._send_json(401, {"message": "unauthorised"})
            return
        query = parse_qs(parsed.query)
        if R_ATTACHMENTS.match(path):
            name = (query.get("fileName") or ["upload.bin"])[0]
            with _LOCK:
                index = STATE["next_attachment"]
                STATE["next_attachment"] += 1
                new_id = _guid(f"upload-{index}")
                ATTACHMENTS[new_id] = {
                    "tag": f"upload-{index}",
                    "id": new_id,
                    "name": name,
                    "bytes": body,
                    "md5": hashlib.md5(body).hexdigest(),
                    "size": len(body),
                    "status": 200,
                    "delay": 0.0,
                }
                BY_TAG[f"upload-{index}"] = ATTACHMENTS[new_id]
                STATE["uploads"].append({"id": new_id, "name": name, "size": len(body),
                                         "md5": hashlib.md5(body).hexdigest()})
            self._record(path, parsed.query, 201, True, {"file_name": name, "bytes": len(body)})
            self._send_json(201, {"id": new_id, "url": attachment_url(f"upload-{index}")})
            return
        self._record(path, parsed.query, 404, True)
        self._send_json(404, {"message": f"no route for POST {path}"})

    def do_DELETE(self) -> None:  # noqa: N802 - stdlib name
        if self._control(urlparse(self.path).path):
            return
        self._send_json(404, {"message": "no route"})


def main() -> None:
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(
        f"ado-mock listening on :{PORT} (pat {PAT[:4]}…, project {PROJECT}, "
        f"{len(ATTACHMENTS)} attachments, public {PUBLIC_URL})",
        flush=True,
    )
    server.serve_forever()


if __name__ == "__main__":
    main()
