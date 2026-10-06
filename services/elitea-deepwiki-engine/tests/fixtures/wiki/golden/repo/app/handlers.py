"""Request handlers, dispatched by name."""

from app.models import Note, make_store

STORE = make_store()

ROUTES = {
    "create": "handle_create",
    "search": "handle_search",
}


def handle_create(note_id, body):
    """Create a note."""
    return STORE.save(Note(note_id, body))


def handle_search(text):
    """Find notes whose body contains text."""
    return [n for n in STORE.items.values() if text in n.body]


def dispatch(route, *args):
    """Call the handler registered for route."""
    handler = globals()[ROUTES[route]]
    return handler(*args)
