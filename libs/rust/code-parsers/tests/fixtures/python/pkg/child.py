from pkg.base import Base, helper
from pkg import base as base_mod


class Child(Base):
    def run(self):
        helper(1)
        repo = Repository()
        return shared() + base_mod.helper(2)


class Other(base_mod.Base, Unknown):
    pass


def shared():
    return Child()
