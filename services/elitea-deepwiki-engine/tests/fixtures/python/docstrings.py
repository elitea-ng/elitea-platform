'''Module docstring.

	Tabbed continuation.
    Spaces.
'''


def raw():
    r"""Raw \n docstring with \d backslashes."""


def escaped():
    "Escapes: \t tab \x41 \101 \u00e9 end"


def concatenated():
    "First part " 'second part'


def fstring():
    f"""Not a docstring {raw}."""


def bytes_doc():
    b"Not a docstring either"


def whitespace():
    """

        Indented first content.
          Deeper.
      
    """


def continuation():
    """Line one \
continued."""


class Empty:
    """"""


def odd():
    """   leading spaces
   less indented
       more indented"""
