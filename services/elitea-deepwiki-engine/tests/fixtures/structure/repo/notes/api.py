"""HTTP handlers for notes — création, recherche."""
from notes.store.db import NoteStore


def handle_create_note(store: NoteStore, body: dict) -> dict:
    """Create a note from a request body."""
    return store.save(body["title"], body.get("text", ""))


def handle_search(store: NoteStore, query: str) -> list:
    return store.search(query)
