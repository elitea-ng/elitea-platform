"""Note models and the store."""

from app.base import BaseStore

MAX_NOTES = 1000


class Note:
    """One note."""

    def __init__(self, note_id, body):
        self.note_id = note_id
        self.body = body


class NoteStore(BaseStore):
    """Stores notes in memory."""

    def save(self, note):
        if self.size() >= MAX_NOTES:
            raise ValueError("full")
        self.items[note.note_id] = note
        return note

    def load(self, note_id):
        return self.items.get(note_id)


def make_store():
    """A new empty store."""
    return NoteStore()
