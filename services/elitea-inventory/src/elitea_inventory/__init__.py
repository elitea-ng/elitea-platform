"""ELITEA Inventory provider engine (ADR-0023 H4c stage I3).

The provider SPI — routes, invocations, admission, the error contract,
composition and artifact upload — is the Go sub-application host's
(``services/elitea-subapp-host``, ``internal/apps/inventory``). What lives here
is the ENGINE: the knowledge-graph ingestion and retrieval layer, copied from
``legacy/plugins/inventory_plugin``, reached from the host as a sidecar over a
Unix socket (``python -m elitea_inventory``).

Importing this package installs the two import shims the copied engine needs:
the Pylon stub its logger imports want, and ``langchain.text_splitter`` under
the name LangChain 1.x moved out from under it. Nothing else happens at import time — in particular the engine
itself is NOT imported, so a container built without the ``engine`` extra still
starts, serves the sidecar protocol and refuses every tool with a reason.
"""

from __future__ import annotations

from .langchain_shim import install as _install_langchain_shim
from .pylon_shim import install as _install_pylon_shim

_install_pylon_shim()
_install_langchain_shim()

__all__: list[str] = []
