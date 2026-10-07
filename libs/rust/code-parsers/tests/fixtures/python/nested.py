class Outer:
    """Outer docstring.

        Indented line.
    \tTabbed.
    """

    LIMIT = 10
    count = 0

    class Inner:
        def __init__(self, engine):
            self.engine = engine
            self.cache = Cache()
            self.client: Optional[Client] = None
            self.store: storage.Store = storage.make()
            self.items = []
            self.cache = Cache()

            def helper():
                self.hidden = Hidden()
            helper()

        def run(self):
            return self.engine.start(Outer.LIMIT)

    after_inner = compute()

    def method(self):
        def local():
            class Local:
                pass
            return Local()
        return local()


def Inner():
    return Outer.Inner(None)


def factory():
    class Made:
        def __init__(self):
            self.x = Outer()
    return Made


class Config:
    database: DatabaseConfig
    cache: Optional[CacheConfig]
    timeout: int = 30
    backend: drivers.Backend

    def __init__(self, database=None):
        self.database = database
        self.backend = drivers.Backend()
        self.extra: Dict[str, Any] = {}


class Outer:
    def __init__(self):
        self.second = Second()
