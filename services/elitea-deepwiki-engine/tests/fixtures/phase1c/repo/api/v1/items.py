from tools import api_tools


class API(api_tools.APIBase):
    url_params = [
        '<int:project_id>',
        '<int:project_id>/<int:item_id>',
    ]

    def get(self, project_id: int, item_id: int | None = None):
        return []

    def post(self, project_id: int):
        return {}
