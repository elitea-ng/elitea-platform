"""Public HTTP API client. Fixtures and turns go through the product API only, never through SQL writes
(DESIGN §8 "Harness writes"). Tokens and cookies stay in memory and never reach evidence files."""
import html.parser
import http.cookiejar
import json
import urllib.error
import urllib.parse
import urllib.request
import uuid

from .common import HarnessError

TIMEOUT_S = 30
MAX_BODY = 8 * 1024 * 1024
APPLICATION_CONTRACT = 'agent.execute.application.v1'


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


class _FormParser(html.parser.HTMLParser):
    """Collects the first form's action, hidden inputs and submit buttons (oidc-provider-mock authorize page)."""

    def __init__(self):
        super().__init__()
        self.action = None
        self.fields = {}
        self.buttons = []
        self._in_form = False

    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if tag == 'form' and self.action is None:
            self._in_form = True
            self.action = a.get('action', '')
        elif self._in_form and tag == 'input':
            if a.get('type') in ('submit', 'button'):
                self.buttons.append((a.get('name'), a.get('value', '')))
            elif a.get('name'):
                self.fields[a['name']] = a.get('value', '')
        elif self._in_form and tag == 'button':
            self.buttons.append((a.get('name'), a.get('value', '')))

    def handle_endtag(self, tag):
        if tag == 'form':
            self._in_form = False


class Client:
    def __init__(self, base_url):
        self.base = base_url.rstrip('/')
        self.jar = http.cookiejar.CookieJar()
        self._opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
        self._raw = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar), _NoRedirect)
        self.token = None

    # ---- transport -----------------------------------------------------------------------------------------
    def request(self, method, path, body=None, *, form=None, headers=None, expect=(200, 201, 204), raw=False):
        url = path if path.startswith('http') else self.base + path
        data = None
        hdrs = {'Accept': 'application/json'}
        if body is not None:
            data = json.dumps(body).encode()
            hdrs['Content-Type'] = 'application/json'
        if form is not None:
            data = urllib.parse.urlencode(form).encode()
            hdrs['Content-Type'] = 'application/x-www-form-urlencoded'
        if self.token:
            hdrs['Authorization'] = 'Bearer ' + self.token
        hdrs.update(headers or {})
        req = urllib.request.Request(url, data=data, method=method, headers=hdrs)
        opener = self._raw if raw else self._opener
        try:
            with opener.open(req, timeout=TIMEOUT_S) as resp:
                status, payload, rhdrs = resp.status, resp.read(MAX_BODY), dict(resp.headers)
        except urllib.error.HTTPError as err:
            status, payload, rhdrs = err.code, err.read(MAX_BODY), dict(err.headers or {})
        if expect and status not in expect:
            raise HarnessError(f'{method} {urllib.parse.urlsplit(url).path} returned {status}')
        if raw:
            return status, payload, rhdrs
        if not payload:
            return status, None
        try:
            return status, json.loads(payload)
        except ValueError:
            return status, payload.decode('utf-8', 'replace')

    # ---- auth ----------------------------------------------------------------------------------------------
    def login_oidc(self, subject):
        """Browser-equivalent OIDC mock login: /auth/oidc/login -> authorize form -> callback cookie."""
        status, _, hdrs = self.request('GET', '/auth/oidc/login', raw=True, expect=None)
        location = hdrs.get('Location') or hdrs.get('location')
        if status not in (302, 303) or not location:
            raise HarnessError(f'OIDC login did not redirect (status {status})')
        status, page, _ = self.request('GET', location, raw=True, expect=(200,))
        form = _FormParser()
        form.feed(page.decode('utf-8', 'replace'))
        fields = dict(form.fields)
        fields['sub'] = subject
        for name, value in form.buttons:
            if name and 'authori' in (value or name).lower():
                fields[name] = value
                break
        action = urllib.parse.urljoin(location, form.action or location)
        status, _, hdrs = self.request('POST', action, form=fields, raw=True, expect=None)
        hops = 0
        while status in (301, 302, 303, 307) and hops < 5:
            nxt = urllib.parse.urljoin(action, hdrs.get('Location') or hdrs.get('location'))
            status, _, hdrs = self.request('GET', nxt, raw=True, expect=None)
            action, hops = nxt, hops + 1
        _, info = self.request('GET', '/auth/info')
        if not isinstance(info, dict) or not info.get('authenticated'):
            raise HarnessError('OIDC login did not produce an authenticated session')
        return info

    def mint_token(self, name):
        _, body = self.request('POST', '/api/v2/auth/token/', {'name': name, 'expires': None, 'project_id': None})
        self.token = body['token']
        return body['uuid']

    def revoke_token(self, token_uuid):
        self.request('DELETE', f'/api/v2/auth/token/{token_uuid}', expect=(200, 204, 404))

    def author(self):
        return self.request('GET', '/api/v2/social/author/')[1]

    # ---- fixtures ------------------------------------------------------------------------------------------
    def create_pipeline(self, project_id, name, yaml_text, description='crash-recovery fixture'):
        body = {'name': name, 'description': description, 'type': 'interface', 'versions': [{
            'name': 'base', 'agent_type': 'pipeline', 'instructions': yaml_text, 'conversation_starters': [],
            'variables': [], 'meta': {'step_limit': 25, 'internal_tools': []}}]}
        _, app = self.request('POST', f'/api/v2/elitea_core/applications/prompt_lib/{project_id}', body)
        return int(app['id']), int(app['version_details']['id'])

    def create_agent(self, project_id, name, instructions, model_name, model_project_id=None, tools=None):
        llm = {'model_name': model_name}
        if model_project_id is not None:
            llm['model_project_id'] = model_project_id
        body = {'name': name, 'description': 'crash-recovery fixture', 'type': 'interface', 'versions': [{
            'name': 'base', 'agent_type': 'openai', 'instructions': instructions, 'conversation_starters': [],
            'variables': [], 'tools': tools or [], 'meta': {'step_limit': 25, 'internal_tools': []},
            'llm_settings': llm}]}
        _, app = self.request('POST', f'/api/v2/elitea_core/applications/prompt_lib/{project_id}', body)
        return int(app['id']), int(app['version_details']['id'])

    def application(self, project_id, app_id):
        return self.request('GET', f'/api/v2/elitea_core/application/prompt_lib/{project_id}/{app_id}')[1]

    def create_conversation(self, project_id, name, user_id, app_id, app_name, version_id, agent_type):
        _, conv = self.request('POST', f'/api/v2/elitea_core/conversations/prompt_lib/{project_id}',
                               {'name': name, 'is_private': True})
        conv_id = int(conv['id'])
        participants = [
            {'entity_name': 'user', 'entity_meta': {'id': int(user_id)}},
            {'entity_name': 'application', 'entity_meta': {'id': app_id, 'name': app_name, 'project_id': project_id},
             'entity_settings': {'version_id': version_id, 'agent_type': agent_type, 'variables': [], 'icon_meta': {}}},
        ]
        _, rows = self.request('POST', f'/api/v2/elitea_core/participants/prompt_lib/{project_id}/{conv_id}',
                               participants)
        rows = rows if isinstance(rows, list) else rows.get('items', rows.get('participants', []))
        app_rows = [r for r in rows if r.get('entity_name') == 'application']
        if len(app_rows) != 1:
            raise HarnessError('the conversation did not return exactly one application participant')
        return {'conversation_id': conv_id, 'conversation_uuid': conv['uuid'], 'participant_id': int(app_rows[0]['id'])}

    # ---- turns ---------------------------------------------------------------------------------------------
    def send(self, project_id, conversation, user_input, question_id=None):
        question_id = question_id or str(uuid.uuid4())
        body = {'project_id': project_id, 'conversation_uuid': conversation['conversation_uuid'],
                'participant_id': conversation['participant_id'], 'question_id': question_id,
                'payload': {'user_input': user_input}}
        path = (f"/api/v2/elitea_core/messages/prompt_lib/{project_id}/{conversation['conversation_uuid']}"
                f'?execution_contract={APPLICATION_CONTRACT}')
        status, resp = self.request('POST', path, body, expect=(200, 201))
        return question_id, status, resp

    def stop(self, project_id, response_message_id):
        status, _ = self.request('DELETE', f'/api/v2/elitea_core/task/prompt_lib/{project_id}/{response_message_id}',
                                 expect=(204, 409))
        return status

    def messages(self, project_id, conversation_id):
        return self.request('GET', f'/api/v2/elitea_core/messages/prompt_lib/{project_id}/{conversation_id}')[1]

    def assistant_answers(self, project_id, conversation_id):
        """Assistant items of a conversation: role, streaming flag, error flag, content (kept in memory only)."""
        body = self.messages(project_id, conversation_id) or {}
        items = body.get('items', body if isinstance(body, list) else [])
        return [i for i in items if i.get('role') == 'assistant']
