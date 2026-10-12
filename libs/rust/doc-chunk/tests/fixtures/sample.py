import json
import os

DEFAULT = 3


def load(path):
    """Read a JSON file."""
    with open(path) as handle:
        return json.load(handle)


class Store:
    def __init__(self, root):
        self.root = root

    def save(self, name, value):
        target = os.path.join(self.root, name)
        with open(target, "w") as handle:
            json.dump(value, handle)
        return target


@staticmethod
def helper(x):
    return x * DEFAULT
