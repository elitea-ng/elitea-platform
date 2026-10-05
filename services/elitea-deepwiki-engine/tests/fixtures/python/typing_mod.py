from typing import Annotated, Any, Callable, Dict, Generic, List, Literal, Optional, TypeVar
import typing
from pydantic import Field

T = TypeVar("T")
K = TypeVar("K")


def simple(a: int, b: str = "x", *args: int, c: bool = False, **kwargs: Any) -> None:
    pass


def generics(a: Dict[str, int], b: Optional[Dict[str, Any]], c: Callable[[int, str], None]) -> List[User]:
    return []


def annotated(name: Annotated[str, Field(description="it's \"quoted\"", default=None, max_length=10)]) -> tuple[int, ...]:
    return (1,)


def unions(x: User | None, y: "Forward", z: typing.List[int], w: Literal['a', 'b']) -> dict[str, list[User]] | None:
    return None


def posonly(a, b, /, c, *, d, e=1):
    return a


def strange(x: -1, y: 1e16, z: 0x1F, q: None, r: ..., s: b"raw", t: 1j) -> "a" "b":
    pass


def defaults(cb=lambda x, *a, k=2, **kw: x + 1, d={"a": 1, **base}, s={1, 2}, n=not True):
    pass


def calls(x: Annotated[int, lambda v: v > 0, [1, 2], {"k": (1,)}, -x ** 2, (a if b else c), f"{x!r:>10}"]):
    pass


def parenthesized(
    value: (
        First
        | Second
    ),
) -> (
    Result
):
    return value


class Repo(Generic[T], Base[K, models.User], metaclass=Meta):
    items: Dict[str, T]

    def get(self, key: K) -> Optional[T]:
        return self.items.get(key)


class Child(mod.sub.Parent[Item]):
    pass


def pep695[T: (int, str), *Ts, **P](x: T, *rest: *Ts) -> T:
    return x


type Alias[V] = list[V] | None

value: Final[int] = 3
other: "Undefined"
