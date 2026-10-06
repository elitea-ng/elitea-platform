"""User routes and a native hash helper."""
import ctypes

from fastapi import APIRouter
from pydantic import BaseModel

lib = ctypes.CDLL("libnative.so")
router = APIRouter(prefix="/users")


class UserPublic(BaseModel):
    id: int
    full_name: str | None = None
    is_active: bool = True


@router.get("/me")
def read_me() -> UserPublic:
    """Return the caller."""
    return UserPublic(id=1)


@router.post("/{user_id}/hash")
def hash_user(user_id: int) -> int:
    return lib.compute_hash(user_id)
