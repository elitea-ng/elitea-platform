from app.users import read_me


class TestUserPublic:
    def test_read_me(self):
        assert read_me().id == 1
