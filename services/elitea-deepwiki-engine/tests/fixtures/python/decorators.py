"""Decorators of every shape."""
import functools
from abc import ABC, abstractmethod


def plain(fn):
    return fn


@plain
@functools.lru_cache(maxsize=None)
@registry["x"]
def decorated(a, b=2):
    return a + b


@dataclass(frozen=True)
@plain
class Point:
    x: int
    y: int = 0
    label: Optional[str] = None
    owner: models.User
    tags: List[Tag]

    @property
    def norm(self) -> float:
        return (self.x ** 2 + self.y ** 2) ** 0.5

    @staticmethod
    def origin() -> "Point":
        return Point(0, 0)

    @classmethod
    def of(cls, *values: int, **extra) -> Point:
        return cls(*values, **extra)

    @norm.setter
    def norm(self, value):
        raise AttributeError(value)


class Shape(ABC):
    @abstractmethod
    async def area(self) -> float: ...

    @functools.cached_property
    def name(self):
        return type(self).__name__
