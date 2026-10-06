"""Base storage."""


class BaseStore:
    """A store that keeps notes by id."""

    def __init__(self):
        self.items = {}

    def size(self):
        """Number of notes."""
        return len(self.items)
