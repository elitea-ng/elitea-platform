"""Generate the provider derivation table from the Python factory.

Mirrors tool_operations.generate_wiki: provider_type/provider_config/
repository/branch/project read from repo_config, then
RepoProviderFactory.from_toolkit_config. The clone URL's userinfo is split
off: the Rust port never puts it in a URL, it sends the Authorization
header git would have sent for it (Basic base64("user:password"), an
absent password being empty, as git/libcurl send it).
"""
import base64, json, sys
from urllib.parse import urlparse
from elitea_deepwiki.engine.repo_providers import RepoProviderFactory

def gh(cfg, repo, branch="main", **kw):
    return dict(provider_type="github", provider_config=cfg, repository=repo, branch=branch, project=None, is_cloud=None, **kw)

C = []
def case(name, rc): C.append((name, rc))

case("github anonymous", gh({}, "owner/repo"))
case("github public api token", gh({"base_url": "https://api.github.com", "access_token": "ghp_tok1"}, "owner/repo"))
case("github token and username", gh({"base_url": "https://api.github.com", "access_token": "ghp_tok1", "username": "octo"}, "owner/repo"))
case("github username and password", gh({"username": "octo", "password": "pw1"}, "owner/repo"))
case("github enterprise api url", gh({"base_url": "https://ghe.example.com/api/v3", "access_token": "ghp_tok1"}, "owner/repo"))
case("github enterprise with port", gh({"base_url": "https://ghe.example.com:8443/api/v3"}, "owner/repo"))
case("github https repository url", gh({}, "https://github.com/owner/repo.git"))
case("github ssh repository", gh({}, "git@github.com:owner/repo.git"))
case("github ssh without suffix", gh({}, "git@github.com:owner/repo"))
case("github git suffix", gh({}, "owner/repo.git"))
case("github slashes", gh({}, "/owner/repo/"))
case("github tree url keeps its path", gh({}, "https://github.com/owner/repo/tree/dev"))
case("github whitespace", gh({}, "  owner/repo  "))
case("github token whitespace stripped", gh({"access_token": "  ghp_tok2  "}, "owner/repo"))
case("github empty token is anonymous", gh({"access_token": ""}, "owner/repo"))
case("github branch with slash", gh({}, "owner/repo", branch="feature/x"))
case("github dotted name", gh({}, "owner/my.git.repo.git"))
case("github repository url host is not the clone host", gh({}, "https://gitlab.com/owner/repo"))
case("github provider alias", dict(gh({}, "owner/repo"), provider_type="GH"))

def gl(cfg, repo, branch="main"):
    return dict(provider_type="gitlab", provider_config=cfg, repository=repo, branch=branch, project=None, is_cloud=None)
case("gitlab token", gl({"url": "https://gitlab.com", "private_token": "glpat-1"}, "group/sub/project"))
case("gitlab self hosted trailing slash", gl({"url": "https://gitlab.example.com/", "private_token": "glpat-1"}, "group/project"))
case("gitlab tree url", gl({}, "https://gitlab.com/group/project/-/tree/main"))
case("gitlab ssh", gl({"url": "https://gitlab.example.com"}, "git@gitlab.example.com:group/sub/project.git"))
case("gitlab anonymous default host", gl({}, "group/project"))
case("gitlab dotted", gl({}, "group/project.git.git"))

def bb(cfg, repo, project=None, branch="main"):
    return dict(provider_type="bitbucket", provider_config=cfg, repository=repo, branch=branch, project=project, is_cloud=None)
case("bitbucket cloud workspace", bb({"url": "https://bitbucket.org", "username": "u1", "password": "app1"}, "repo-name", "workspace"))
case("bitbucket cloud full path", bb({"url": "https://bitbucket.org", "username": "u1", "password": "app1"}, "ws/repo", "workspace"))
case("bitbucket server project", bb({"url": "https://bitbucket.example.com", "username": "u1", "password": "pw1"}, "repo", "PROJ"))
case("bitbucket server url repository", bb({"url": "https://bitbucket.example.com/"}, "https://bitbucket.example.com/scm/PROJ/repo.git"))
case("bitbucket api host", bb({"url": "https://api.bitbucket.org/2.0"}, "ws/repo"))
case("bitbucket anonymous default", bb({}, "ws/repo"))
case("bitbucket ssh", bb({}, "git@bitbucket.org:ws/repo.git"))
case("bitbucket server ssh scm", bb({"url": "https://bitbucket.example.com"}, "git@bitbucket.example.com:scm/PROJ/repo.git"))
case("bitbucket username without password", bb({"username": "u1"}, "ws/repo"))

def ado(cfg, repo, project=None, branch="main", ptype="ado_repos"):
    return dict(provider_type=ptype, provider_config=cfg, repository=repo, branch=branch, project=project, is_cloud=None)
case("ado token", ado({"organization_url": "https://dev.azure.com/myorg", "project": "MyProject", "token": "adopat1"}, "my-repo", "MyProject"))
case("ado encoded names", ado({"organization_url": "https://dev.azure.com/myorg/", "project": "My Project", "token": "adopat1"}, "my repo", "My Project"))
case("ado visualstudio", ado({"organization_url": "https://myorg.visualstudio.com", "token": "adopat1"}, "my-repo", "Proj"))
case("ado full url repository", ado({"organization_url": "https://dev.azure.com/myorg"}, "https://dev.azure.com/myorg/MyProject/_git/my-repo", "MyProject"))
case("ado project from configuration", ado({"organization_url": "https://dev.azure.com/myorg", "project": "CfgProj"}, "r", None))
case("ado alias", ado({"organization_url": "https://dev.azure.com/myorg"}, "r", "P", ptype="azure"))
case("ado missing organization", ado({}, "r", "P"))
case("ado bad organization", ado({"organization_url": "https://example.com/myorg"}, "r", "P"))
case("ado missing project", ado({"organization_url": "https://dev.azure.com/myorg"}, "r", None))
case("unknown provider", dict(gh({}, "o/r"), provider_type="svn"))

out = []
for name, rc in C:
    entry = {"name": name, "repo_config": rc}
    try:
        cc = RepoProviderFactory.from_toolkit_config(
            provider_type=rc.get("provider_type", "github"),
            config=rc.get("provider_config", {}),
            repository=rc.get("repository"),
            branch=rc.get("branch", "main"),
            project=rc.get("project"),
        )
        p = urlparse(cc.clone_url)
        netloc = p.netloc.rsplit("@", 1)[-1]
        auth = None
        if "@" in p.netloc:
            user = p.username or ""
            pw = p.password or ""
            auth = "Basic " + base64.b64encode(f"{user}:{pw}".encode()).decode()
        entry["expected"] = {
            "provider": cc.provider.value,
            "host": cc.host,
            "url": f"{p.scheme}://{netloc}{p.path}",
            "repo_identifier": cc.repo_identifier,
            "branch": cc.branch,
            "auth_method": cc.auth_method,
            "authorization": auth,
        }
    except Exception as e:  # noqa: BLE001
        entry["error"] = {"type": type(e).__name__, "message": str(e)}
    out.append(entry)
json.dump(out, sys.stdout, indent=2)
print()
