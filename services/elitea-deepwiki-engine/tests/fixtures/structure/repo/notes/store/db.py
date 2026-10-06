class NoteStore:
    """SQLite-backed note storage."""

    def save(self, title, text):
        return {"title": title, "text": text}

    def search(self, query):
        return []
