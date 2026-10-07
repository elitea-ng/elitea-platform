"""The notes REST API."""

from tools import api_tools


class API(api_tools.APIBase):
    url_params = ["<int:project_id>"]

    def get(self, project_id):
        return {"project": project_id}

    def post(self, project_id):
        return {"created": project_id}
