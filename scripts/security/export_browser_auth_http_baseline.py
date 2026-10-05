#!/usr/bin/env python3
"""Export bounded current-baseline browser and Form authentication evidence.

The catalog is deliberately narrower than complete pylon_auth parity.  It pins
the effective Auth Core/Form/Auth Init browser flow, Pylon server-side session
semantics, tracked Centry composition, and the EliteaUI consumers that constrain
the first Go HTTP adapter.  OIDC, SAML, full IdP/provider behavior, and
administrative APIs remain separate migration slices.

No Pylon or product dependency is imported.  Python sources are inspected with
the standard-library AST, configuration is reduced to an explicit allowlist,
and credential values are never emitted.
"""

from __future__ import annotations

import argparse
import ast
import hashlib
import json
import os
import subprocess
import sys
from pathlib import Path
from typing import Any, Callable


SCHEMA_VERSION = 1
FORM_CREDENTIAL_ENVIRONMENT_SENTINEL = "environment_expansion"
FORM_CREDENTIAL_INLINE_SENTINEL = "inline_value_present_but_redacted"

AUTH_CORE_FILES = (
    "config.yml",
    "methods/auth_context.py",
    "methods/auth_processors.py",
    "methods/auth_providers.py",
    "methods/credential_handlers.py",
    "methods/hooks.py",
    "methods/info_mappers.py",
    "methods/public_rules.py",
    "methods/redirects.py",
    "methods/replies.py",
    "methods/success_mappers.py",
    "methods/urls.py",
    "module.py",
    "requirements.txt",
    "routes/auth.py",
    "rpc/auth_processors.py",
    "rpc/auth_providers.py",
    "rpc/group_permissions.py",
    "rpc/groups.py",
    "rpc/info_mappers.py",
    "rpc/public_rules.py",
    "rpc/success_mappers.py",
    "rpc/roles.py",
    "rpc/user_groups.py",
    "rpc/user_providers.py",
    "rpc/users.py",
)
AUTH_FORM_FILES = (
    "admin_schema.json",
    "form_export.py",
    "module.py",
    "routes/form.py",
    "templates/login.html",
)
AUTH_INIT_FILES = (
    "config.yml",
    "module.py",
    "rpc/processor.py",
)
AUTH_IDP_RPC_FILES = (
    "methods/auth_tools.py",
    "module.py",
    "rpc/authorize.py",
)
AUTH_MAPPERS_FILES = (
    "config.yml",
    "methods/tools.py",
    "module.py",
    "requirements.txt",
    "rpc/header.py",
    "rpc/json.py",
    "rpc/noop.py",
)
MAIN_AUTH_FILES = ("module.py",)
RUNTIME_INTERFACE_LITELLM_FILES = (
    "config.yml",
    "methods/init.py",
)
ARTIFACTS_FILES = ("methods/s3.py",)
ELITEA_CORE_FILES = (
    "config.yml",
    "module.py",
)
PYLON_FILES = (
    "pylon/core/tools/exposure.py",
    "pylon/core/tools/app.py",
    "pylon/core/tools/app_shim.py",
    "pylon/core/tools/module/descriptor.py",
    "pylon/core/tools/session.py",
    "pylon/core/tools/server/init.py",
    "pylon/core/tools/server/gevent.py",
    "pylon/core/tools/server/wsgi.py",
    "pylon/core/tools/web.py",
    "pylon/framework/router/init.py",
    "pylon/framework/router/router.py",
    "pylon/main.py",
    "requirements.txt",
    "version.txt",
)
UI_FILES = (
    "README.md",
    "src/[fsd]/app/routes/router.jsx",
    "src/[fsd]/features/auth/lib/constants/auth.constants.js",
    "src/[fsd]/features/auth/lib/helpers/auth.helpers.js",
    "src/[fsd]/features/auth/lib/helpers/authPopup.helpers.js",
    "src/[fsd]/pages/auth/index.jsx",
    "src/[fsd]/pages/settings/index.jsx",
    "src/[fsd]/widgets/sidebar-root/ui/button/UserButton.jsx",
    "src/api/eliteaApi.js",
    "src/api/auth.js",
    "src/routes.js",
)
ADMIN_UI_FILES = (
    "frontend/src/components/Layout/Sidebar.jsx",
    "module.py",
)
CENTRY_FULL_FILES: tuple[str, ...] = ()
CENTRY_SELECTED_FILES = (
    "docker-compose.yml",
    "envs/default.env",
    "pylon_auth/configs/auth_core.yml",
    "pylon_auth/configs/auth_form.yml",
    "pylon_auth/configs/auth_init.yml",
    "pylon_auth/pylon.yml",
    "pylon_main/configs/auth.yml",
    "pylon_main/configs/shared.yml",
    "pylon_main/pylon.yml",
)
OPTIONAL_RUNTIME_CONFIG_FILES = ("pylon_auth/configs/auth_mappers.yml",)

EXPECTED_ROUTES = {
    "auth_core/routes/auth.py#Route.auth": ("/auth", ["GET"]),
    "auth_core/routes/auth.py#Route.info": ("/info", ["GET"]),
    "auth_core/routes/auth.py#Route.login": ("/login", ["GET"]),
    "auth_core/routes/auth.py#Route.logout": ("/logout", ["GET"]),
    "auth_form/routes/form.py#Route.authorize": ("/authorize", ["POST"]),
    "auth_form/routes/form.py#Route.login": ("/login", ["GET"]),
    "auth_form/routes/form.py#Route.logout": ("/logout", ["GET"]),
}

FINGERPRINT_TARGETS = {
    "auth_core": {
        "methods/auth_context.py": (
            ("Method", "get_auth_context"),
            ("Method", "get_auth_reference"),
            ("Method", "set_auth_context"),
        ),
        "methods/auth_processors.py": (("Method", "auth_processors_init"),),
        "methods/auth_providers.py": (("Method", "auth_providers_init"),),
        "methods/credential_handlers.py": (("Method", "credential_handlers_init"),),
        "methods/hooks.py": (
            ("Method", "after_request_hook"),
            ("Method", "before_request_hook"),
            ("Method", "error_handler"),
            ("Method", "hooks_init"),
        ),
        "methods/info_mappers.py": (("Method", "info_mappers_init"),),
        "methods/public_rules.py": (("Method", "public_rule_matches"),),
        "methods/redirects.py": (
            ("Method", "access_needed_redirect"),
            ("Method", "access_success_redirect"),
            ("Method", "logout_needed_redirect"),
            ("Method", "logout_success_redirect"),
        ),
        "methods/replies.py": (
            ("Method", "access_denied_reply"),
            ("Method", "access_success_reply"),
        ),
        "methods/success_mappers.py": (("Method", "success_mappers_init"),),
        "methods/urls.py": (
            ("Method", "get_relative_url_prefix"),
            ("Method", "make_source_url"),
            ("Method", "sign_target_url"),
            ("Method", "verify_target_url"),
        ),
        "module.py": (("Module", "init"),),
        "routes/auth.py": (
            ("Route", "auth"),
            ("Route", "info"),
            ("Route", "login"),
            ("Route", "logout"),
        ),
        "rpc/auth_processors.py": (
            ("RPC", "register_auth_processor"),
            ("RPC", "unregister_auth_processor"),
        ),
        "rpc/auth_providers.py": (
            ("RPC", "register_auth_provider"),
            ("RPC", "unregister_auth_provider"),
        ),
        "rpc/group_permissions.py": (("RPC", "add_group_permission"),),
        "rpc/groups.py": (
            ("RPC", "add_group"),
            ("RPC", "get_group"),
        ),
        "rpc/info_mappers.py": (
            ("RPC", "register_info_mapper"),
            ("RPC", "unregister_info_mapper"),
        ),
        "rpc/public_rules.py": (
            ("RPC", "add_public_rule"),
            ("RPC", "remove_public_rule"),
        ),
        "rpc/success_mappers.py": (
            ("RPC", "noop_success_mapper"),
            ("RPC", "register_success_mapper"),
            ("RPC", "rpc_success_mapper"),
        ),
        "rpc/roles.py": (
            ("RPC", "assign_user_to_role"),
            ("RPC", "get_token_permissions"),
            ("RPC", "get_user_permissions"),
            ("RPC", "get_user_roles"),
        ),
        "rpc/user_groups.py": (("RPC", "add_user_group"),),
        "rpc/user_providers.py": (
            ("RPC", "add_user_provider"),
            ("RPC", "get_user_from_provider"),
        ),
        "rpc/users.py": (
            ("RPC", "add_user"),
            ("RPC", "get_user"),
            ("RPC", "update_user"),
        ),
    },
    "auth_form": {
        "module.py": (
            ("Module", "deinit"),
            ("Module", "init"),
        ),
        "routes/form.py": (
            ("Route", "authorize"),
            ("Route", "login"),
            ("Route", "logout"),
        ),
    },
    "auth_idp_rpc": {
        "methods/auth_tools.py": (
            ("Method", "check_authorization_header"),
            ("Method", "check_credential_data"),
        ),
        "module.py": (("Module", "init"),),
        "rpc/authorize.py": (("RPC", "authorize"),),
    },
    "auth_init": {
        "module.py": (("Module", "init"),),
        "rpc/processor.py": (("RPC", "init_auth_processor"),),
    },
    "auth_mappers": {
        "methods/tools.py": (("Method", "have_requirement"),),
        "module.py": (("Module", "init"),),
        "rpc/header.py": (("RPC", "header_success_mapper"),),
        "rpc/json.py": (
            ("RPC", "json_info_mapper"),
            ("RPC", "json_success_mapper"),
        ),
        "rpc/noop.py": (("RPC", "noop_info_mapper"),),
    },
    "main_auth": {
        "module.py": (
            ("Module", "__init__"),
            ("Module", "_after_request_hook"),
            ("Module", "_before_request_hook"),
            ("Module", "_get_auth_cache_key"),
            ("Module", "_get_cached_auth"),
            ("Module", "_make_public_g_auth"),
            ("Module", "_set_cached_auth"),
            ("Module", "add_public_rule"),
        ),
    },
    "runtime_interface_litellm": {
        "methods/init.py": (
            ("Method", "init"),
            ("Method", "deinit"),
        ),
    },
    "admin_ui": {
        "module.py": (("Module", "init"),),
    },
    "artifacts": {
        "methods/s3.py": (
            ("Method", "s3_api_init"),
            ("Method", "s3_api_deinit"),
        ),
    },
    "elitea_core": {
        "module.py": (
            ("Module", "init"),
            ("Module", "mcp_sse_init"),
            ("Module", "mcp_sse_deinit"),
            ("Module", "elitea_ui_init"),
        ),
    },
    "pylon": {
        "pylon/core/tools/app.py": (
            ("AppManager", "__init__"),
            ("AppManager", "add_app_router"),
            ("AppManager", "make_app_instance"),
            ("AppManager", "register_app_hook"),
        ),
        "pylon/core/tools/app_shim.py": (
            ("AppShim", "__getattr__"),
            ("AppShim", "__make_hook_decorator"),
        ),
        "pylon/core/tools/module/descriptor.py": (
            ("ModuleDescriptor", "__init__"),
            ("ModuleDescriptor", "init_all"),
            ("ModuleDescriptor", "init_blueprint"),
            ("ModuleDescriptor", "make_blueprint"),
        ),
        "pylon/framework/router/router.py": (
            ("Router", "error_handler_hook"),
        ),
        "pylon/core/tools/session.py": (
            ("PickleSerializer", "decode"),
            ("PickleSerializer", "encode"),
        ),
        "pylon/core/tools/server/wsgi.py": (
            ("RouterApp", "__call__"),
            ("RouterApp", "__init__"),
        ),
    },
}


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _file_sha256(path: Path) -> str:
    return _sha256(path.read_bytes())


def _canonical_sha256(value: Any) -> str:
    encoded = json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    return _sha256(encoded)


def _run_git(repo: Path, *args: str, binary: bool = False) -> bytes | str:
    result = subprocess.run(
        ["git", "-C", str(repo), *args],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=not binary,
    )
    if result.returncode != 0:
        error = result.stderr.decode() if binary else result.stderr
        raise ValueError(f"git {' '.join(args)} failed for {repo}: {error.strip()}")
    return result.stdout


def _repo(path: Path) -> Path:
    return Path(str(_run_git(path, "rev-parse", "--show-toplevel")).strip()).resolve()


def _head_bytes(repo: Path, relative: str) -> bytes:
    return bytes(_run_git(repo, "show", f"HEAD:{relative}", binary=True))


def _ref_bytes(repo: Path, ref: str, relative: str) -> bytes:
    return bytes(_run_git(repo, "show", f"{ref}:{relative}", binary=True))


def _ref_commit(repo: Path, ref: str) -> str:
    return str(_run_git(repo, "rev-parse", f"{ref}^{{commit}}")).strip()


def _nul_paths(raw: bytes) -> set[str]:
    return {
        os.fsdecode(item)
        for item in raw.split(b"\0")
        if item and not Path(os.fsdecode(item)).is_absolute()
    }


def _dirty_paths(repo: Path) -> set[str]:
    changed: set[str] = set()
    for options in ((), ("--cached",)):
        changed.update(
            _nul_paths(
                bytes(
                    _run_git(
                        repo,
                        "diff",
                        "--name-only",
                        "--no-ext-diff",
                        "-z",
                        *options,
                        "HEAD",
                        "--",
                        ".",
                        binary=True,
                    )
                )
            )
        )
    changed.update(
        _nul_paths(
            bytes(
                _run_git(
                    repo,
                    "ls-files",
                    "--others",
                    "--exclude-standard",
                    "-z",
                    binary=True,
                )
            )
        )
    )
    return changed


def _provenance(
    repo: Path,
    source_root: str,
    full_paths: set[str],
    selected_matchers: dict[str, Callable[[str], Any]] | None = None,
) -> dict[str, Any]:
    dirty = _dirty_paths(repo)
    contract_dirty = sorted(full_paths.intersection(dirty))
    selected_matches_head: dict[str, bool] = {}
    for relative, selector in sorted((selected_matchers or {}).items()):
        worktree_value = selector((repo / relative).read_text(encoding="utf-8"))
        head_value = selector(_head_bytes(repo, relative).decode("utf-8"))
        matches = worktree_value == head_value
        selected_matches_head[relative] = matches
        if not matches:
            contract_dirty.append(relative)
    contract_dirty = sorted(set(contract_dirty))
    return {
        "contract_source_dirty_paths": contract_dirty,
        "contract_sources_reconstructable_from_pinned_head": not contract_dirty,
        "pinned_head": str(_run_git(repo, "rev-parse", "HEAD")).strip(),
        "selected_contract_matches_pinned_head": selected_matches_head,
        "source_ref": "HEAD",
        "source_root": source_root,
    }


def _provenance_at_ref(repo: Path, source_root: str, ref: str) -> dict[str, Any]:
    return {
        "contract_source_dirty_paths": [],
        "contract_sources_reconstructable_from_pinned_head": True,
        "pinned_head": _ref_commit(repo, ref),
        "selected_contract_matches_pinned_head": {},
        "source_ref": ref,
        "source_root": source_root,
    }


def _parse(path: Path) -> ast.Module:
    try:
        return ast.parse(path.read_text(encoding="utf-8"), filename=path.as_posix())
    except (SyntaxError, UnicodeDecodeError) as exc:
        raise ValueError(f"cannot parse {path}: {exc}") from exc


def _parse_ref(repo: Path, ref: str, relative: str) -> ast.Module:
    try:
        text = _ref_bytes(repo, ref, relative).decode("utf-8")
        return ast.parse(text, filename=f"{repo}@{ref}:{relative}")
    except (SyntaxError, UnicodeDecodeError) as exc:
        raise ValueError(f"cannot parse {repo}@{ref}:{relative}: {exc}") from exc


def _class(tree: ast.Module, name: str) -> ast.ClassDef:
    for node in tree.body:
        if isinstance(node, ast.ClassDef) and node.name == name:
            return node
    raise ValueError(f"class {name} not found")


def _method(
    tree: ast.Module,
    class_name: str,
    method_name: str,
) -> ast.FunctionDef | ast.AsyncFunctionDef:
    for node in _class(tree, class_name).body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name == method_name:
            return node
    raise ValueError(f"method {class_name}.{method_name} not found")


def _function(tree: ast.Module, name: str) -> ast.FunctionDef | ast.AsyncFunctionDef:
    for node in tree.body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name == name:
            return node
    raise ValueError(f"function {name} not found")


def _fingerprint(node: ast.AST) -> dict[str, Any]:
    normalized = ast.dump(node, annotate_fields=True, include_attributes=False)
    return {"ast_sha256": _sha256(normalized.encode()), "line": node.lineno}


def _route_spec(node: ast.FunctionDef | ast.AsyncFunctionDef) -> tuple[str, list[str]]:
    route_calls = []
    for decorator in node.decorator_list:
        if not isinstance(decorator, ast.Call) or not isinstance(decorator.func, ast.Attribute):
            continue
        owner = decorator.func.value
        if not isinstance(owner, ast.Name) or owner.id != "web" or decorator.func.attr != "route":
            continue
        route_calls.append(decorator)
    if len(route_calls) != 1:
        raise ValueError(f"expected one web.route decorator on {node.name}")
    call = route_calls[0]
    if not call.args or not isinstance(call.args[0], ast.Constant) or not isinstance(call.args[0].value, str):
        raise ValueError(f"route path on {node.name} is not a literal string")
    methods = ["GET"]
    for keyword in call.keywords:
        if keyword.arg != "methods":
            continue
        try:
            methods = ast.literal_eval(keyword.value)
        except (TypeError, ValueError) as exc:
            raise ValueError(f"route methods on {node.name} are not literal") from exc
        if not isinstance(methods, list) or not all(isinstance(item, str) for item in methods):
            raise ValueError(f"route methods on {node.name} must be a literal list")
    return call.args[0].value, sorted(method.upper() for method in methods)


def _yaml_scalars(text: str) -> dict[str, str]:
    """Return simple mapping scalars; secrets are filtered by callers."""
    result: dict[str, str] = {}
    stack: list[tuple[int, str]] = []
    for raw_line in text.splitlines():
        line = raw_line.split("#", 1)[0].rstrip()
        if not line.strip() or line.lstrip().startswith("-"):
            continue
        indent = len(line) - len(line.lstrip())
        stripped = line.strip()
        if ":" not in stripped:
            continue
        key, value = stripped.split(":", 1)
        while stack and stack[-1][0] >= indent:
            stack.pop()
        path = ".".join([item[1] for item in stack] + [key.strip()])
        value = value.strip()
        if not value:
            stack.append((indent, key.strip()))
            continue
        result[path] = value.strip('"').strip("'")
    return result


def _yaml_list(text: str, path: str) -> list[str]:
    stack: list[tuple[int, str]] = []
    active_indent: int | None = None
    values: list[str] = []
    for raw_line in text.splitlines():
        line = raw_line.split("#", 1)[0].rstrip()
        if not line.strip():
            continue
        indent = len(line) - len(line.lstrip())
        stripped = line.strip()
        if stripped.startswith("-"):
            if active_indent is not None and indent >= active_indent:
                values.append(stripped[1:].strip().strip('"').strip("'"))
            continue
        if ":" not in stripped:
            continue
        key, value = stripped.split(":", 1)
        while stack and stack[-1][0] >= indent:
            stack.pop()
        current = ".".join([item[1] for item in stack] + [key.strip()])
        if current == path and not value.strip():
            active_indent = indent
        elif active_indent is not None and indent <= active_indent:
            active_indent = None
        if not value.strip():
            stack.append((indent, key.strip()))
    return values


def _yaml_mapping_paths_and_scalars(text: str) -> tuple[set[str], dict[str, str]]:
    """Read only ordinary mapping paths and scalar literals from simple YAML."""
    paths: set[str] = set()
    scalars: dict[str, str] = {}
    stack: list[tuple[int, str]] = []
    for raw_line in text.splitlines():
        line = raw_line.split("#", 1)[0].rstrip()
        if not line.strip() or line.lstrip().startswith("-"):
            continue
        indent = len(line) - len(line.lstrip())
        stripped = line.strip()
        if ":" not in stripped:
            continue
        key, raw_value = stripped.split(":", 1)
        while stack and stack[-1][0] >= indent:
            stack.pop()
        path = ".".join([item[1] for item in stack] + [key.strip()])
        paths.add(path)
        raw_value = raw_value.strip()
        if not raw_value:
            stack.append((indent, key.strip()))
            continue
        if raw_value.startswith('"') and raw_value.endswith('"'):
            try:
                value = json.loads(raw_value)
            except json.JSONDecodeError as exc:
                raise ValueError(f"invalid double-quoted YAML scalar at {path}") from exc
        elif raw_value.startswith("'") and raw_value.endswith("'"):
            value = raw_value[1:-1].replace("''", "'")
        else:
            value = raw_value
        if not isinstance(value, str):
            raise ValueError(f"non-string YAML scalar at {path}")
        scalars[path] = value
    return paths, scalars


def _tracked_auth_mapper_contract(auth_mappers_root: Path) -> dict[str, Any]:
    text = (auth_mappers_root / "config.yml").read_text(encoding="utf-8")
    paths, scalars = _yaml_mapping_paths_and_scalars(text)
    header_path = "header.scopes.grafana.headers.X-WEBAUTH-USER"
    json_path = "json.scopes.galloper.login"
    if header_path not in scalars or json_path not in scalars:
        raise ValueError("tracked auth mapper projection is incomplete")

    requirements = [
        line.strip()
        for line in (auth_mappers_root / "requirements.txt").read_text(
            encoding="utf-8"
        ).splitlines()
        if line.strip() and not line.lstrip().startswith("#")
    ]
    jsonpath = [line for line in requirements if line.startswith("jsonpath_rw")]
    if len(jsonpath) != 1:
        raise ValueError("tracked auth mapper JSONPath dependency is ambiguous")

    return {
        "header": {
            "outputs": {"X-WEBAUTH-USER": scalars[header_path]},
            "requirements_present": "header.scopes.grafana.require" in paths,
            "scope": "grafana",
        },
        "json": {
            "endpoint": scalars.get("json.endpoint"),
            "outputs": {"login": scalars[json_path]},
            "scope": "galloper",
        },
        "jsonpath_dependency": jsonpath[0],
    }


def _env_allowlist(text: str, names: tuple[str, ...]) -> dict[str, str]:
    allowed = set(names)
    result: dict[str, str] = {}
    for raw_line in text.splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        if key in allowed:
            result[key] = value
    if set(result) != allowed:
        raise ValueError(f"missing environment defaults: {sorted(allowed.difference(result))}")
    return result


def _selected_auth_pylon(text: str) -> dict[str, Any]:
    values = _yaml_scalars(text)
    keys = (
        "server.path",
        "server.proxy.x_for",
        "server.proxy.x_host",
        "server.proxy.x_proto",
        "sessions.prefix",
        "application.APPLICATION_ROOT",
        "application.PERMANENT_SESSION_LIFETIME",
        "application.PREFERRED_URL_SCHEME",
        "application.SERVER_NAME",
        "application.SESSION_COOKIE_DOMAIN",
        "application.SESSION_COOKIE_HTTPONLY",
        "application.SESSION_COOKIE_NAME",
        "application.SESSION_COOKIE_PATH",
        "application.SESSION_COOKIE_SECURE",
    )
    selected = {key: values.get(key) for key in keys}
    selected["application.SESSION_COOKIE_SAMESITE"] = values.get(
        "application.SESSION_COOKIE_SAMESITE"
    )
    selected["application.SECRET_KEY_configured"] = "application.SECRET_KEY" in values
    selected["exposure.event_node.type"] = values.get("exposure.event_node.type")
    selected["exposure.expose"] = values.get("exposure.expose")
    selected["rpc.redis_configured"] = any(
        key.startswith("rpc.redis.") for key in values
    )
    selected["sessions.redis_configured"] = any(
        key.startswith("sessions.redis.") for key in values
    )
    return selected


def _selected_main_pylon(text: str) -> dict[str, Any]:
    values = _yaml_scalars(text)
    return {
        "exposure.event_node.type": values.get("exposure.event_node.type"),
        "exposure.handle.enabled": values.get("exposure.handle.enabled"),
        "exposure.handle.prefixes": _yaml_list(text, "exposure.handle.prefixes"),
    }


def _selected_runtime_services(text: str) -> dict[str, str]:
    selected_services = {"pylon_auth", "pylon_main"}
    service: str | None = None
    result: dict[str, str] = {}
    for raw_line in text.splitlines():
        line = raw_line.split("#", 1)[0].rstrip()
        if not line.strip():
            continue
        indent = len(line) - len(line.lstrip())
        stripped = line.strip()
        if indent == 2 and stripped.endswith(":"):
            service = stripped[:-1]
            continue
        if service not in selected_services:
            continue
        if indent == 4 and stripped.startswith("image:"):
            result[f"{service}.image"] = stripped.split(":", 1)[1].strip()
        elif stripped.startswith("- PYLON_WEB_RUNTIME="):
            result[f"{service}.web_runtime"] = stripped.split("=", 1)[1].strip()
    expected_keys = {
        "pylon_auth.image",
        "pylon_auth.web_runtime",
        "pylon_main.image",
        "pylon_main.web_runtime",
    }
    if set(result) != expected_keys:
        raise ValueError(
            f"missing reviewed runtime service selectors: {sorted(expected_keys.difference(result))}"
        )
    return result


def _selected_default_env(text: str) -> dict[str, str]:
    return _env_allowlist(
        text,
        ("APP_PROTO", "COOKIES_LIFETIME", "COOKIES_SECURE", "NAME_PREFIX"),
    )


def _selected_auth_core_config(text: str) -> dict[str, Any]:
    values = _yaml_scalars(text)
    keys = (
        "auth_denied_url",
        "auth_provider",
        "db_options.isolation_level",
        "default_login_url",
        "default_logout_url",
        "register_error_handler",
        "url_prefix",
    )
    selected = {key: values.get(key) for key in keys}
    selected["additional_headers"] = {
        key.removeprefix("additional_headers."): value
        for key, value in sorted(values.items())
        if key.startswith("additional_headers.")
    }
    selected["allow_auth_traversal"] = values.get("allow_auth_traversal")
    selected["other_auth_headers"] = {
        key.removeprefix("other_auth_headers."): value
        for key, value in sorted(values.items())
        if key.startswith("other_auth_headers.")
    }
    return selected


def _effective_auth_core_config(auth_core_root: Path, centry_root: Path) -> dict[str, Any]:
    base = _selected_auth_core_config(
        (auth_core_root / "config.yml").read_text(encoding="utf-8")
    )
    override = _selected_auth_core_config(
        (centry_root / "pylon_auth/configs/auth_core.yml").read_text(encoding="utf-8")
    )
    effective = dict(base)
    for key, value in override.items():
        if key in {"additional_headers", "other_auth_headers"}:
            effective[key] = {**base.get(key, {}), **value}
        elif value is not None:
            effective[key] = value
    if effective.get("allow_auth_traversal") is None:
        effective["allow_auth_traversal"] = True
    if effective.get("register_error_handler") is None:
        effective["register_error_handler"] = True
    if effective.get("url_prefix") is None:
        effective["url_prefix"] = "/"
    return effective


def _selected_auth_init_config(text: str) -> dict[str, Any]:
    return {
        "initial_global_admins": _yaml_list(text, "initial_global_admins"),
        "initial_root_permissions": _yaml_list(text, "initial_root_permissions"),
    }


def _effective_auth_init_config(auth_init_root: Path, centry_root: Path) -> dict[str, Any]:
    base = _selected_auth_init_config(
        (auth_init_root / "config.yml").read_text(encoding="utf-8")
    )
    override = _selected_auth_init_config(
        (centry_root / "pylon_auth/configs/auth_init.yml").read_text(encoding="utf-8")
    )
    return {
        key: override[key] if override[key] else base[key]
        for key in sorted(base)
    }


def _selected_main_auth_config(text: str) -> dict[str, Any]:
    values = _yaml_scalars(text)
    public_rules: list[str] = []
    in_public_rules = False
    for raw_line in text.splitlines():
        line = raw_line.split("#", 1)[0].rstrip()
        if not line.strip():
            continue
        indent = len(line) - len(line.lstrip())
        stripped = line.strip()
        if not in_public_rules:
            if indent == 0 and stripped == "public_rules:":
                in_public_rules = True
            continue
        if indent == 0 and not stripped.startswith("-"):
            break
        if not stripped.startswith("- uri:"):
            continue
        public_rules.append(stripped.split(":", 1)[1].strip().strip('"').strip("'"))
    return {
        "auth_mode": values.get("auth_mode"),
        "public_uri_rules": public_rules,
    }


def _selected_litellm_public_config(text: str) -> dict[str, str | None]:
    return {"url_prefix": _yaml_scalars(text).get("url_prefix")}


def _selected_elitea_core_public_config(text: str) -> dict[str, bool]:
    value = _yaml_scalars(text).get("public_messages_route")
    if value not in {"true", "false"}:
        raise ValueError("elitea_core public_messages_route must be an explicit boolean")
    return {"public_messages_route": value == "true"}


def _selected_elitea_core_public_override(text: str) -> dict[str, bool | None]:
    value = _yaml_scalars(text).get("public_messages_route")
    if value is None:
        return {"public_messages_route": None}
    if value not in {"true", "false"}:
        raise ValueError("elitea_core public_messages_route override must be boolean")
    return {"public_messages_route": value == "true"}


def _main_public_rule_contract(
    main_auth: dict[str, Any],
    litellm_config: dict[str, str | None],
    elitea_core_config: dict[str, bool],
) -> dict[str, Any]:
    configured_patterns = main_auth.get("public_uri_rules")
    expected_configured_patterns = [
        "/forward\\-auth/.*",
        "/applications/application_icon.*",
        "/datasources/datasource_icon.*",
        "/prompt_lib/prompt_icon.*",
    ]
    if configured_patterns != expected_configured_patterns:
        raise ValueError("reviewed ordered Main configured public-rule set changed")
    if litellm_config != {"url_prefix": "/llm"}:
        raise ValueError("reviewed LiteLLM public prefix changed")
    if elitea_core_config != {"public_messages_route": True}:
        raise ValueError("reviewed elitea_core public messages setting changed")

    configured = [
        {
            "id": identifier,
            "ordinal": ordinal,
            "rule": {"uri": pattern},
            "source": f"centry/pylon_main/configs/auth.yml#public_rules[{ordinal}]",
        }
        for ordinal, (identifier, pattern) in enumerate(
            zip(
                (
                    "config.browser_auth",
                    "config.application_icon",
                    "config.datasource_icon",
                    "config.prompt_icon",
                ),
                expected_configured_patterns,
                strict=True,
            )
        )
    ]
    dynamic = [
        {
            "add_source": "runtime_interface_litellm/methods/init.py#Method.init",
            "condition": "plugin initialized with tracked url_prefix=/llm",
            "effective_in_tracked_base_config": True,
            "id": "runtime_interface_litellm.proxy",
            "plugin_local_order": 1,
            "remove_source": "runtime_interface_litellm/methods/init.py#Method.deinit",
            "rule": {"uri": "/llm/.*"},
        },
        {
            "add_source": "admin_ui/module.py#Module.init",
            "condition": "plugin initialized",
            "effective_in_tracked_base_config": True,
            "id": "admin_ui.static_assets",
            "plugin_local_order": 1,
            "remove_source": None,
            "rule": {
                "uri": "/admin/app/.*\\.(js|css|ico|png|jpg|jpeg|gif|svg|woff|woff2|ttf|eot|map)$"
            },
        },
        {
            "add_source": "artifacts/methods/s3.py#Method.s3_api_init",
            "condition": "S3 API init hook runs",
            "effective_in_tracked_base_config": True,
            "id": "artifacts.s3_sigv4",
            "plugin_local_order": 1,
            "remove_source": "artifacts/methods/s3.py#Method.s3_api_deinit",
            "rule": {"uri": "/artifacts/s3/.*"},
        },
        {
            "add_source": "elitea_core/module.py#Module.elitea_ui_init",
            "condition": "elitea_core UI init runs",
            "effective_in_tracked_base_config": True,
            "id": "elitea_core.socket_io",
            "plugin_local_order": 1,
            "remove_source": None,
            "rule": {"uri": "/socket\\.io/.*"},
        },
        {
            "add_source": "elitea_core/module.py#Module.elitea_ui_init",
            "condition": "elitea_core UI init runs",
            "effective_in_tracked_base_config": True,
            "id": "elitea_core.robots",
            "plugin_local_order": 2,
            "remove_source": None,
            "rule": {"uri": "/robots\\.txt"},
        },
        {
            "add_source": "elitea_core/module.py#Module.elitea_ui_init",
            "condition": "elitea_core UI init runs",
            "effective_in_tracked_base_config": True,
            "id": "elitea_core.favicon",
            "plugin_local_order": 3,
            "remove_source": None,
            "rule": {"uri": "/favicon\\.ico"},
        },
        {
            "add_source": "elitea_core/module.py#Module.elitea_ui_init",
            "condition": "elitea_core UI init runs",
            "effective_in_tracked_base_config": True,
            "id": "elitea_core.access_denied",
            "plugin_local_order": 4,
            "remove_source": None,
            "rule": {"uri": "/app/access_denied"},
        },
        {
            "add_source": "elitea_core/module.py#Module.mcp_sse_init",
            "condition": "tracked base config public_messages_route=true",
            "effective_in_tracked_base_config": True,
            "id": "elitea_core.public_messages",
            "plugin_local_order": 5,
            "remove_source": "elitea_core/module.py#Module.mcp_sse_deinit",
            "rule": {
                "uri": "/elitea_core/[0-9]+/messages\\?session_id=.+"
            },
        },
        {
            "add_source": "elitea_core/module.py#Module.init",
            "condition": "elitea_core main init reaches webhook registration",
            "effective_in_tracked_base_config": True,
            "id": "elitea_core.webhook",
            "plugin_local_order": 6,
            "remove_source": None,
            "rule": {
                "uri": "/api/v2/elitea_core/webhook/prompt_lib/[0-9]+/[0-9]+/(github|gitlab|custom)"
            },
        },
    ]
    return {
        "auth_core_direct": {
            "initial_rules": [],
            "reviewed_dynamic_registration_sites": [],
        },
        "main_local": {
            "configured_registration_order": configured,
            "dynamic_registration_sites": dynamic,
            "main_rpc_mode_forwards_rules_to_auth_core": False,
            "ordering": {
                "configured": (
                    "Auth Module.init appends the YAML rules in the listed order"
                ),
                "dynamic": (
                    "each site appends during its plugin lifecycle; cross-plugin scheduler "
                    "order is not inferred, while plugin_local_order pins order within a plugin"
                ),
                "evaluation": (
                    "Main evaluates every rule in its local insertion-ordered list into one "
                    "public boolean without short-circuiting; each mapping requires every "
                    "regex value to fullmatch its corresponding source field"
                ),
            },
        },
    }


def _selected_main_shared_config(text: str) -> dict[str, str]:
    values = _yaml_scalars(text)
    allow_cors = values.get("settings.allow_cors")
    if allow_cors not in {"true", "false"}:
        raise ValueError("tracked pylon_main allow_cors must be an explicit boolean")
    return {"settings.allow_cors": allow_cors}


def _safe_form_config(text: str) -> dict[str, Any]:
    users: list[set[str]] = []
    current: set[str] | None = None
    credential_source_sentinels: list[str] = []
    in_users = False
    user_entry_indent: int | None = None
    for raw_line in text.splitlines():
        line = raw_line.split("#", 1)[0].rstrip()
        if not line.strip():
            continue
        indent = len(line) - len(line.lstrip())
        stripped = line.strip()
        if not in_users:
            if indent == 0 and stripped == "users:":
                in_users = True
            continue
        if indent == 0 and not stripped.startswith("-"):
            break
        if stripped.startswith("- ") and ":" in stripped and indent == 0:
            current = set()
            users.append(current)
            user_entry_indent = indent
            key, value = stripped[2:].split(":", 1)
            current.add(key.strip())
            if key.strip() == "password":
                credential_source_sentinels.append(
                    FORM_CREDENTIAL_ENVIRONMENT_SENTINEL
                    if _is_environment_expansion(value)
                    else FORM_CREDENTIAL_INLINE_SENTINEL
                )
            continue
        if (
            current is not None
            and user_entry_indent is not None
            and indent == user_entry_indent + 2
            and ":" in stripped
        ):
            key, value = stripped.split(":", 1)
            current.add(key.strip())
            if key.strip() == "password":
                credential_source_sentinels.append(
                    FORM_CREDENTIAL_ENVIRONMENT_SENTINEL
                    if _is_environment_expansion(value)
                    else FORM_CREDENTIAL_INLINE_SENTINEL
                )
    if not users:
        raise ValueError("tracked Form configuration has no users")
    return {
        "configured_user_count": len(users),
        "configured_user_keys": [sorted(user) for user in users],
        # These are fixed classification sentinels. Credential bytes are
        # discarded above and never reach the deterministic evidence digest.
        "credential_sources": credential_source_sentinels,
        "credential_values_exported": False,
    }


def _is_environment_expansion(value: str) -> bool:
    unquoted = value.strip().strip('"').strip("'")
    return unquoted.startswith("${") and unquoted.endswith("}")


def _tracked_literal_paths(
    repo: Path,
    literal: str,
    prefixes: tuple[str, ...],
) -> list[str]:
    result = subprocess.run(
        ["git", "-C", str(repo), "grep", "-l", "-F", "-e", literal, "--", "."],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    if result.returncode not in {0, 1}:
        raise ValueError(f"git grep failed for {repo}: {result.stderr.strip()}")
    return sorted(
        path.removeprefix("./")
        for path in result.stdout.splitlines()
        if path.removeprefix("./").startswith(prefixes)
    )


def _join_url_path(*parts: str) -> str:
    segments = [part.strip("/") for part in parts if part and part.strip("/")]
    return "/" + "/".join(segments)


def _requirement_version(text: str, package: str) -> str:
    values = []
    for raw_line in text.splitlines():
        line = raw_line.split("#", 1)[0].strip()
        if line.startswith(f"{package}=="):
            values.append(line.split("==", 1)[1])
    if len(values) != 1:
        raise ValueError(f"expected one pinned {package} requirement, found {len(values)}")
    return values[0]


def _route_composition(deployment: dict[str, Any]) -> dict[str, Any]:
    auth_server_path = deployment["pylon_auth_selected"]["server.path"]
    context_url_prefix = auth_server_path.rstrip("/") or "/"
    exposure_prefixes = deployment["pylon_main"]["browser_auth_exposure"][
        "exposure.handle.prefixes"
    ]
    if len(exposure_prefixes) != 1:
        raise ValueError("reviewed browser-auth exposure must have exactly one prefix")
    external_base_path = exposure_prefixes[0].rstrip("/") or "/"
    if external_base_path != context_url_prefix:
        raise ValueError(
            "pylon_main exposure prefix no longer matches the exposed pylon_auth server path"
        )
    auth_core_prefix = deployment["auth_core"]["url_prefix"]
    auth_form_prefix = _join_url_path(auth_core_prefix, "auth_form")
    return {
        "auth_core_blueprint_prefix": auth_core_prefix,
        "auth_form_blueprint_prefix": auth_form_prefix,
        "auth_form_descriptor_name": "auth_form",
        "auth_pylon_context_url_prefix": context_url_prefix,
        "auth_pylon_server_path": auth_server_path,
        "composition_chain": [
            "pylon_main Auth before_request attempts auth_authorize; the configured /auth public rule keeps negative decisions non-blocking",
            "pylon_main exposure accepts the configured /auth prefix",
            "exposure forwards the original WSGI path to the pylon_auth process",
            "pylon_auth strips its context URL prefix before app-router dispatch",
            "ModuleDescriptor registers Auth Core at its configured blueprint prefix",
            "Auth Form derives /auth_form from Auth Core get_relative_url_prefix",
            "Flask combines the blueprint prefix with the literal route decorator",
        ],
        "external_base_path": external_base_path,
        "external_listener_methods": [
            "DELETE",
            "GET",
            "HEAD",
            "OPTIONS",
            "PATCH",
            "POST",
            "PUT",
        ],
        "main_exposure_prefixes": exposure_prefixes,
        "pylon_runtime_ref": deployment["runtime_composition"]["pylon_source_ref"],
        "pylon_web_runtime": deployment["runtime_composition"][
            "pylon_auth.web_runtime"
        ],
        "root_auth_alias": False,
    }


def _framework_contract(
    auth_core_root: Path,
    pylon_repo: Path,
    pylon_ref: str,
) -> dict[str, Any]:
    pylon_requirements = _ref_bytes(pylon_repo, pylon_ref, "requirements.txt").decode(
        "utf-8"
    )
    auth_requirements = (auth_core_root / "requirements.txt").read_text(encoding="utf-8")
    return {
        "pinned_versions": {
            "Flask": _requirement_version(pylon_requirements, "Flask"),
            "Flask-Session": _requirement_version(pylon_requirements, "Flask-Session"),
            "PyJWT": _requirement_version(auth_requirements, "PyJWT"),
            "Werkzeug": _requirement_version(pylon_requirements, "Werkzeug"),
            "gevent": _requirement_version(pylon_requirements, "gevent"),
        },
        "pylon_runtime_commit": _ref_commit(pylon_repo, pylon_ref),
        "pylon_runtime_ref": pylon_ref,
        "route_method_semantics": (
            "Flask supplies HEAD for GET routes and automatic OPTIONS; the outer Pylon "
            "exposure listener accepts seven methods before inner Flask dispatch"
        ),
        "route_normalization_dependency": (
            "Pylon's exposure subpath rule contains a double slash and relies on "
            "Werkzeug Map merge_slashes=True to serve the single-slash external path"
        ),
        "werkzeug_merge_slashes": True,
        "unsupported_method_semantics": (
            "inner Werkzeug MethodNotAllowed is handled by Auth Core's generic exception "
            "handler, which returns the configured access-denied redirect response with "
            "its status overridden to 400"
        ),
    }


def _http_outcomes(
    deployment: dict[str, Any], composition: dict[str, Any]
) -> list[dict[str, Any]]:
    base = composition["external_base_path"]
    denied = deployment["auth_core"]["auth_denied_url"]
    routes = {
        "auth": _join_url_path(base, "auth"),
        "form_authorize": _join_url_path(base, "auth_form", "authorize"),
        "form_login": _join_url_path(base, "auth_form", "login"),
        "form_logout": _join_url_path(base, "auth_form", "logout"),
        "info": _join_url_path(base, "info"),
        "login": _join_url_path(base, "login"),
        "logout": _join_url_path(base, "logout"),
    }
    return [
        {
            "id": "application.common_headers",
            "scope": "responses produced by managed pylon_auth Flask applications",
            "headers": {"Server": "Centry"},
            "dynamic_headers_excluded": ["Content-Length", "Date"],
        },
        {
            "id": "browser_auth.missing_forwarded_header",
            "request": {"method": "GET", "route": routes["auth"]},
            "response": {
                "body": "Flask redirect HTML",
                "location": denied,
                "status": 302,
            },
        },
        {
            "id": "browser_auth.rpc_success",
            "request": {
                "method": "GET",
                "route": routes["auth"],
                "target": "rpc",
            },
            "response": {
                "body": "OK",
                "headers": ["X-Auth-ID", "X-Auth-Reference", "X-Auth-Type"],
                "status": 200,
            },
        },
        {
            "id": "browser_auth.noop_success",
            "request": {
                "method": "GET",
                "route": routes["auth"],
                "target": None,
            },
            "response": {"body": "OK", "identity_headers": [], "status": 200},
        },
        {
            "id": "browser_auth.empty_target",
            "request": {
                "method": "GET",
                "precondition": (
                    "a credential, browser session, or public-rule branch reaches the "
                    "success mapper"
                ),
                "route": routes["auth"],
                "target": "",
            },
            "response": {"location": denied, "status": 302},
        },
        {
            "id": "browser_auth.unknown_target",
            "request": {
                "method": "GET",
                "route": routes["auth"],
                "target": "unregistered",
            },
            "response": {"location": denied, "status": 302},
        },
        {
            "id": "browser_auth.invalid_credential",
            "request": {"method": "GET", "route": routes["auth"]},
            "code_capable_branches": [
                "matching public rule plus an accepted target mapper returns its success response",
                "otherwise redirect 302 to the configured access-denied URL",
            ],
            "browser_session_traversed": False,
            "tracked_effective_response": {
                "location": denied,
                "reason": "reviewed Auth Core public-rule set is empty",
                "status": 302,
            },
        },
        {
            "id": "browser_auth.no_authentication",
            "request": {"method": "GET", "route": routes["auth"]},
            "tracked_effective_branches": [
                "a done and unexpired browser session returns 200 through the selected success mapper",
                "otherwise redirect 302 to the absolute configured provider login route with target_to",
            ],
            "code_capability_not_effective_in_reviewed_config": (
                "a matching Auth Core public rule could return 200, but no reviewed registration populates that set"
            ),
        },
        {
            "id": "browser.login",
            "request": {"method": "GET", "route": routes["login"]},
            "branches": [
                "configured Form provider redirects 302 to the absolute Form login route with target_to",
                "missing provider or URL construction failure redirects 302 to the configured access-denied URL",
            ],
        },
        {
            "id": "browser.logout",
            "request": {"method": "GET", "route": routes["logout"]},
            "branches": [
                "selected provider redirects 302 to its logout route or URL with target_to",
                "missing provider or URL construction failure redirects 302 to the configured access-denied URL",
            ],
        },
        {
            "id": "browser.info.raw",
            "request": {"method": "GET", "route": routes["info"], "target": None},
            "response": {"content_type": "application/json", "status": 200},
        },
        {
            "id": "browser.info.empty_target",
            "request": {"method": "GET", "route": routes["info"], "target": ""},
            "response": {"location": denied, "status": 302},
        },
        {
            "id": "browser.info.json",
            "request": {"method": "GET", "route": routes["info"], "target": "json"},
            "response": {"content_type": "application/json", "status": 200},
        },
        {
            "id": "browser.info.denied",
            "request": {
                "method": "GET",
                "route": routes["info"],
                "target": "unregistered or mapper failure",
            },
            "response": {"location": denied, "status": 302},
        },
        {
            "id": "form.login",
            "request": {"method": "GET", "route": routes["form_login"]},
            "response": {"content_type": "text/html", "status": 200},
        },
        {
            "id": "form.authorize.invalid",
            "request": {"method": "POST", "route": routes["form_authorize"]},
            "response": {
                "location": f'{routes["form_login"]}?error=true',
                "status": 302,
            },
        },
        {
            "id": "form.authorize.success",
            "request": {"method": "POST", "route": routes["form_authorize"]},
            "response": {
                "location": "verified target URL, or configured default_login_url",
                "status": 302,
            },
        },
        {
            "id": "form.authorize.processor_failure",
            "request": {"method": "POST", "route": routes["form_authorize"]},
            "response": {"location": denied, "status": 302},
            "state": (
                "the denial saves the prewritten done=true session context without rotation; a later unexpired browser check accepts it as user and maps unresolved user_id to '-'"
            ),
        },
        {
            "id": "form.logout",
            "request": {"method": "GET", "route": routes["form_logout"]},
            "response": {
                "location": "verified target URL, or configured default_logout_url",
                "status": 302,
            },
        },
        {
            "id": "exposure.unsupported_inner_method",
            "request": {
                "method": "an outer-listener method absent from the inner route",
                "route": "any composed browser/Form route",
            },
            "response": {
                "body": "Flask redirect HTML",
                "location": denied,
                "status": 400,
            },
            "evidence_kind": "source-derived; no live Flask/Traefik execution",
        },
        {
            "id": "exposure.outer_unsupported_method",
            "request": {
                "method": "a method outside the outer listener allowlist, for example TRACE",
                "route": f"{base}/...",
            },
            "response": {"status": 405},
            "evidence_kind": "source-derived; no live Flask/Traefik execution",
        },
        {
            "id": "inner.automatic_options",
            "request": {"method": "OPTIONS", "route": "any composed route"},
            "response": {
                "allow": "the route's inner_flask_methods set",
                "status": 200,
            },
        },
        {
            "id": "main.cors_options_replacement",
            "request": {
                "method": "OPTIONS",
                "scope": "pylon_main while tracked shared allow_cors is true",
            },
            "response": {
                "body": "empty replacement response",
                "headers": {
                    "Access-Control-Allow-Credentials": "true",
                    "Access-Control-Allow-Headers": "*",
                    "Access-Control-Allow-Methods": "*",
                    "Access-Control-Allow-Origin": "*",
                },
                "status": 200,
            },
            "side_effects": [
                "global authorization still executes before the replacement",
                "the original status, body, and headers are discarded",
                "configured additional headers applied before replacement are lost",
            ],
        },
        {
            "id": "inner.head",
            "request": {"method": "HEAD", "route": "a composed GET route"},
            "response": {
                "body": "suppressed",
                "status_and_headers": "the GET handler outcome",
            },
            "side_effects": "the GET handler still executes",
        },
        {
            "id": "inner.not_found",
            "request": {"route": "inner missing route or non-normalized trailing-slash case"},
            "response": {"body": "Not Found", "status": 404},
        },
        {
            "id": "exposure.registry_miss",
            "request": {"route": f"{base}/..."},
            "response": {"status": 404},
        },
        {
            "id": "exposure.timeout",
            "request": {"route": f"{base}/..."},
            "response": {"status": 504},
        },
    ]


def _source_hashes(
    roots: dict[str, Path],
    files: dict[str, tuple[str, ...]],
) -> dict[str, str]:
    result: dict[str, str] = {}
    for label, relative_files in sorted(files.items()):
        for relative in relative_files:
            path = roots[label] / relative
            if not path.is_file():
                raise ValueError(f"missing {label} evidence source: {relative}")
            result[f"{label}/{relative}"] = _file_sha256(path)
    return result


def _ref_source_hashes(
    label: str,
    repo: Path,
    ref: str,
    relative_files: tuple[str, ...],
) -> dict[str, str]:
    return {
        f"{label}/{relative}": _sha256(_ref_bytes(repo, ref, relative))
        for relative in relative_files
    }


def _optional_runtime_config_inventory(centry_root: Path) -> list[dict[str, Any]]:
    """Record only whether optional runtime overrides exist, never their bytes."""
    return [
        {
            "contents_exported": False,
            "path": f"centry/{relative}",
            "present": (centry_root / relative).is_file(),
        }
        for relative in OPTIONAL_RUNTIME_CONFIG_FILES
    ]


def _behavior_contracts() -> list[dict[str, Any]]:
    return [
        {
            "id": "browser.auth.get",
            "method": "GET",
            "route": "/auth/auth",
            "input": {
                "required_headers": [
                    "X-Forwarded-Method",
                    "X-Forwarded-Proto",
                    "X-Forwarded-Host",
                    "X-Forwarded-Uri",
                    "X-Forwarded-For",
                ],
                "query": ["scope", "target"],
            },
            "target_query_semantics": {
                "absent": (
                    "Flask returns None; None selects the registered built-in no-op mapper"
                ),
                "explicit_empty": (
                    "Flask returns an empty string; it is distinct from None and has no "
                    "mapper in the reviewed composition, so access is denied"
                ),
            },
            "precedence": [
                "Authorization credential handler",
                "configured additional credential headers in mapping order",
                "unexpired browser session",
                "public rules in registration order",
                "clear authentication context and redirect to login",
            ],
            "browser_success": {
                "auth_id": "stored user_id converted to string, or '-' when unresolved",
                "auth_reference": "raw browser session cookie value",
                "auth_type": "user",
            },
            "unauthenticated": (
                "sign proto://host+uri from forwarded headers and redirect through the configured provider"
            ),
        },
        {
            "id": "browser.login.get",
            "method": "GET",
            "route": "/auth/login",
            "input": {"query": {"target_to": "optional signed target token"}},
            "side_effects": ["clear authentication context in the current server-side session"],
            "outcome": (
                "dispatch configured auth_provider; Form redirects to /auth/auth_form/login"
            ),
            "default_target": "signed configured default_login_url",
        },
        {
            "id": "form.login.get",
            "method": "GET",
            "route": "/auth/auth_form/login",
            "input": {
                "query": {
                    "error": "presence, regardless of value, shows the invalid-credential alert",
                    "target_to": "missing becomes the empty string",
                }
            },
            "outcome": {
                "content": "configured login template, default login.html",
                "form_action": "/auth/auth_form/authorize",
                "hidden_fields": ["target"],
                "visible_fields": ["login", "password"],
            },
        },
        {
            "id": "form.authorize.post",
            "method": "POST",
            "route": "/auth/auth_form/authorize",
            "input": {
                "content_type": "HTML form",
                "fields": {
                    "login": "missing becomes empty string",
                    "password": "missing becomes empty string",
                    "target": "missing becomes empty string",
                },
            },
            "matching": (
                "iterate configured users in order and accept the first exact Python string equality match on login and password"
            ),
            "configuration_shape": {
                "admin_schema_required_fields": ["email", "login", "password"],
                "runtime_match_fields": ["login", "password"],
                "runtime_optional_claim_source": "nested attributes object",
                "top_level_email_consumed_by_route": False,
            },
            "success": {
                "authentication_expiration": "naive datetime.now() plus exactly 86400 seconds",
                "provider": "form",
                "provider_reference": "matched login",
                "provider_attributes": "configured user.attributes or empty object",
                "sessionindex": "current raw server-side session cookie/reference",
                "user_resolution": (
                    "best-effort get_user_from_provider(login); every exception becomes user_id=None"
                ),
                "processor_order": "registered auth processors execute sequentially before session rotation",
                "session": "regenerate identifier, then save processed authentication context",
                "redirect": "verified target token URL, or configured default_login_url on any verification error",
            },
            "failure": {
                "location": "/auth/auth_form/login?error=true",
                "target_preserved": False,
            },
        },
        {
            "id": "form.resolved_snapshot_bridge",
            "activation": "ELITEA_AUTH_FORM_USERS_FILE is present",
            "input": (
                "auth_form descriptor.config after Pylon base/settings/custom merge "
                "and environment/vault/tunable substitution"
            ),
            "payload": {
                "encoding": "canonical compact UTF-8 JSON",
                "shape": {"users": "ordered closed login/password/email/attributes records"},
                "maximum_bytes": 1048576,
                "maximum_users": 256,
                "excluded": ["login_template", "all other descriptor configuration"],
            },
            "publication": {
                "parent": "absolute canonical process-owned 0700 directory",
                "file_mode": "0600",
                "algorithm": "same-directory temp, file fsync, atomic replace, best-effort directory fsync",
                "errors": "generic and value/path-free",
            },
            "lifecycle": {
                "preflight": "before route/provider registration",
                "publish": "after successful route/provider registration",
                "publish_failure": "unregister provider and deinitialize descriptor",
                "deinit": "unregister provider without deleting the last complete snapshot",
                "in_place_reconfiguration": "unsupported; restart or plugin reload required",
            },
            "target_gate": (
                "elitea-auth-validate securely reads the file and invokes the exact Go "
                "FormProvider parser before target startup"
            ),
        },
        {
            "id": "browser.logout.get",
            "method": "GET",
            "route": "/auth/logout",
            "input": {"query": {"target_to": "optional signed target token"}},
            "provider_selection": (
                "session provider when registered, otherwise configured default provider"
            ),
            "default_target": "signed configured default_logout_url",
            "outcome": "dispatch provider logout route or URL",
        },
        {
            "id": "form.logout.get",
            "method": "GET",
            "route": "/auth/auth_form/logout",
            "input": {"query": {"target_to": "missing becomes empty string"}},
            "side_effects": [
                "destroy old server-side session",
                "regenerate a session identifier",
                "clear authentication context",
            ],
            "outcome": (
                "redirect to verified target token URL, or configured default_logout_url on any verification error"
            ),
        },
        {
            "id": "browser.info.get",
            "method": "GET",
            "route": "/auth/info",
            "input": {"query": ["scope", "target"]},
            "outcome": {
                "target_absent": "registered no-op mapper returns the raw six-field authentication context as JSON",
                "target_empty": (
                    "the explicit empty string is distinct from absent None and is "
                    "unregistered, so it redirects to the configured access-denied URL"
                ),
                "target_json": "registered JSON mapper returns raw context plus configured JSONPath projections for scope",
                "target_unknown": (
                    "302 redirect to the configured access-denied URL; mapper failure has the same outcome"
                ),
            },
            "local_auth_check": False,
        },
        {
            "id": "browser.target_token",
            "algorithm": "HS256 JWT using the Flask application secret",
            "payload": {"url": "arbitrary string"},
            "claims_absent": ["aud", "exp", "iat", "iss", "jti", "nbf"],
            "verification": "signature/algorithm check followed by direct url field return",
        },
        {
            "id": "browser.server_session",
            "storage": {
                "backend": "Redis",
                "record_encoding": "Python Pickle despite the initially selected msgpack format",
                "configured_key_prefix": "${NAME_PREFIX}_auth_session_",
            },
            "identifier": {
                "random_bytes": 32,
                "cookie_signed_with_application_secret": True,
            },
            "lifetime": {
                "permanent_by_default": True,
                "tracked_seconds": 604800,
                "refresh_each_request": False,
                "form_authentication_seconds": 86400,
            },
            "authentication_context_fields": [
                "done",
                "error",
                "expiration",
                "provider",
                "provider_attr",
                "user_id",
            ],
        },
        {
            "id": "browser.auth_init.processor",
            "business_rules": [
                "provider reference is provider_attr.nameid",
                "email is attributes.email or <provider-reference>@centry.user, then lowercase",
                "name prefers given_name plus family_name, then attributes.name, then email",
                "when user_id is absent, first link an existing same-email user; otherwise create user, provider link, and root-group membership",
                "attempt new_ai_user on every processor invocation reaching the event, including returning users and before update/role work; the event can carry user_id=None and does not prove login success",
                "update last_login; update name only when update_user reports no returning name",
                "assign super_admin only when provider reference is configured in initial_global_admins and the user currently has no global roles",
            ],
            "failure_branches": [
                "the broad same-email lookup/link exception treats database lookup failure as user absence and may attempt a duplicate create",
                "when same-email lookup succeeds but provider linking fails, local user_id remains non-null while auth_ctx.user_id remains null; creation is skipped and later event/update work receives null",
                "create, provider-link, root-group membership, event, update, and role calls are separate effects under AUTOCOMMIT; a later failure can leave earlier durable effects committed",
                "any propagated processor error redirects to access denied after Form already stored done=true, without clearing or rotating that session context",
            ],
            "startup_failure_branches": [
                "Auth Init treats every root-group lookup exception as absence, then performs separate group and permission writes",
                "Auth Init treats every system-user lookup exception as absence, then performs separate user, group-membership, and super-admin assignment writes",
                "dependency failures can therefore trigger duplicate bootstrap attempts or leave partially initialized durable state",
            ],
        },
        {
            "id": "browser.cors_options_replacement",
            "main_effective_behavior": {
                "enabled_by": "tracked pylon_main settings.allow_cors=true",
                "ordering": [
                    "global authorization executes first",
                    "configured additional headers are applied to the original response",
                    "the OPTIONS branch replaces that response with a new blank 200 response",
                    "wildcard CORS headers are added to the replacement",
                ],
                "lost_response_state": ["body", "status", "previous headers"],
            },
            "auth_core_code_capability": (
                "when Auth Core ALLOW_CORS is truthy for an API preflight, its equivalent "
                "replacement occurs after tracked Server: Centry is applied, so the new "
                "response loses that Server header"
            ),
            "security_defect": (
                "Access-Control-Allow-Origin '*' is combined with "
                "Access-Control-Allow-Credentials true"
            ),
        },
        {
            "id": "browser.main_rpc_authorize",
            "transport": "pylon_main calls auth_authorize over Redis RPC in tracked auth_mode=rpc",
            "cache_contract": {
                "cache_failures": "fall through to auth_authorize RPC",
                "cached_results": "auth_ok=true only; denials and redirects are not cached",
                "key_material": "Authorization header first, otherwise named or fallback *_session cookie",
                "key_omissions": [
                    "HTTP method",
                    "request URI",
                    "target",
                    "scope",
                    "public-rule or authorization-policy revision",
                ],
                "key_transform": "SHA-256 hex truncated to 32 characters, prefixed auth:token: or auth:session:",
                "migration": (
                    "do not port this cache: auth_authorize consumes request and policy "
                    "inputs that the key does not bind, while the key contains only "
                    "credential or session material"
                ),
                "revocation_delay_seconds": 60,
                "ttl_seconds": 60,
            },
            "local_public_override": {
                "classification_time": "before cache lookup and auth_authorize RPC",
                "rpc_short_circuited": False,
                "successful_authentication_wins": True,
                "negative_authorization_result": (
                    "a matching local public rule replaces the negative result with a "
                    "synthetic public principal"
                ),
                "transport_exception": (
                    "becomes a synthetic public principal regardless of whether a local "
                    "public rule matched"
                ),
            },
            "evaluation_sequence": [
                "Authorization credential handler",
                "configured additional credential headers",
                "public-rule classification",
                "referenced browser session",
                "public success when no valid session",
                "login redirect or invalid-credential denial",
            ],
            "credential_failure_divergence": (
                "allow_auth_traversal defaults true: an invalid credential can continue to a valid browser session; without a valid session a matching public rule succeeds, otherwise the request is denied rather than redirected"
            ),
            "http_route_difference": (
                "Auth Core HTTP /auth/auth immediately delegates invalid credentials to access_denied_reply(source): a matching public rule with an accepted target mapper succeeds, otherwise it denies, and it never traverses the browser session"
            ),
            "migration_transport": (
                "after pylon_main and pylon_auth merge, auth_authorize becomes a direct "
                "typed in-process call; the internal Redis RPC and its Redis success cache "
                "disappear, while HTTP /auth/auth remains the ingress contract"
            ),
            "transport_failure": (
                "on a cache miss, pylon_main catches any auth_authorize RPC-call exception, installs a synthetic public principal regardless of local public rules, and continues; permission-decorated handlers may still deny that principal, while undecorated/public-principal flows proceed"
            ),
        },
    ]


def _security_dispositions() -> list[dict[str, str]]:
    return [
        {
            "id": "form.csrf",
            "baseline": "no CSRF validation; source contains an explicit TODO",
            "migration": "correct",
            "requirement": "one-time CSPRNG transaction bound atomically to provider and originating server-side session",
        },
        {
            "id": "form.brute_force",
            "baseline": "no attempt throttling; source contains an explicit TODO",
            "migration": "correct",
            "requirement": "bounded account and source-rate admission without leaking account existence",
        },
        {
            "id": "form.credentials",
            "baseline": "password is expanded into plugin configuration and compared as plaintext",
            "migration": "correct",
            "requirement": "typed secret-backed verifier; never persist, log, return, or place credential bytes in session/transaction records",
        },
        {
            "id": "target.redirect",
            "baseline": "long-lived signed arbitrary URL with no audience, issuer, expiry, or one-time use",
            "migration": "correct",
            "requirement": "bounded same-origin absolute path stored in the one-time transaction; preserve the path and query required by UI",
        },
        {
            "id": "session.serialization",
            "baseline": "Redis value is Python Pickle and is not independently authenticated",
            "migration": "correct",
            "requirement": "strict, versioned, bounded JSON; never decode Pickle in Go",
        },
        {
            "id": "session.fixation",
            "baseline": "session regenerates after processors complete",
            "migration": "preserve_and_strengthen",
            "requirement": "atomically rotate only the server-created originating session after verification and provisioning",
        },
        {
            "id": "session.identity",
            "baseline": "browser success can forward user_id '-' after best-effort provider lookup",
            "migration": "correct",
            "requirement": "transactional provisioning must produce an active authoritative principal before authenticated session rotation",
        },
        {
            "id": "session.processor_failure",
            "baseline": "Form stores done=true before processors; processor denial does not clear or rotate that context, so a later browser check can observe it",
            "migration": "correct",
            "requirement": "keep pre-verification state unauthenticated and publish authenticated state only in the successful atomic rotation after all provisioning completes",
        },
        {
            "id": "identity.provider_namespace",
            "baseline": "provider_ref lookup is global and the Form login string is not namespaced by provider",
            "migration": "correct",
            "requirement": "bind identity lookup to provider plus provider reference while preserving current Form user outcomes through an explicit migration",
        },
        {
            "id": "session.cookie",
            "baseline": "HttpOnly and configured Secure; SameSite is omitted; fixed lifetime is 604800 seconds in tracked defaults",
            "migration": "preserve_and_strengthen",
            "requirement": "opaque ID only, matching cookie/Redis fixed TTL, explicit SameSite, exact-name/path/domain deletion, Secure required outside explicit local development",
        },
        {
            "id": "proxy.trust",
            "baseline": "ProxyFix trusts one forwarded hop and target URL uses forwarded proto/host verbatim",
            "migration": "correct",
            "requirement": "honor forwarded metadata only from configured trusted proxy CIDRs; normalize and validate client IP, proto, and host before policy evaluation; construct redirects from canonical public origin plus validated path; preserve valid trusted-proxy clients and validate the cutover",
        },
        {
            "id": "wire.unsupported_method",
            "baseline": "an outer-allowed method absent from the inner route returns the configured access-denied redirect HTML and Location header with status overwritten to 400",
            "migration": "correct",
            "requirement": "the Go plain-400 response is an intentional invalid-request wire correction, not byte-for-byte parity; preserve status 400 and all valid-client behavior, inventory any client relying on the redirect body or Location, and validate the cutover",
        },
        {
            "id": "credential.basic_decoding",
            "baseline": "Python base64.b64decode uses its permissive default before UTF-8 decoding and splitting the first colon",
            "migration": "correct",
            "requirement": "strict canonical Base64 and bounded UTF-8 credential parsing may reject malformed encodings the current decoder tolerates; preserve valid Basic clients and validate real client encodings before cutover",
        },
        {
            "id": "input.duplicates",
            "baseline": "Flask header and query get calls consume one value and do not explicitly reject duplicate security-sensitive forwarded headers, Authorization, target, or scope",
            "migration": "correct",
            "requirement": "reject ambiguous duplicate security-sensitive headers and query parameters while preserving single-valued valid clients; treat this as an intentional correction requiring migration tests rather than byte-for-byte parity",
        },
        {
            "id": "public_rules.regex_engine",
            "baseline": "current Main and Auth Core compile Python regular expressions and require fullmatch for each rule field",
            "migration": "correct",
            "requirement": "Go RE2 deliberately excludes Python backreferences and look-around; validate every configured and dynamic rule before activation, reject unsupported expressions explicitly, and prove the tracked valid rules retain the same matches",
        },
        {
            "id": "cors.options_replacement",
            "baseline": (
                "tracked Main CORS handling replaces every OPTIONS response after global "
                "authorization and emits wildcard origin with credentials; the equivalent "
                "Auth Core branch also drops its previously applied Server: Centry header"
            ),
            "migration": "correct",
            "requirement": (
                "preserve security/default headers and response ownership, emit credentials "
                "only for an explicitly allowed concrete origin, and never combine wildcard "
                "origin with credentials"
            ),
        },
        {
            "id": "dependency.failure",
            "baseline": (
                "broad exception handling converts some storage/provider failures to denial, unresolved identity, or duplicate-provision attempts; critically, on an authorization cache miss pylon_main converts any auth_authorize RPC-call failure into a synthetic public principal and continues regardless of local public rules"
            ),
            "migration": "correct",
            "requirement": "typed fail-closed errors; dependency outages are distinguishable and never downgrade to anonymous or partial authentication",
        },
        {
            "id": "rpc.boundary",
            "baseline": "pylon_main reaches pylon_auth through the Redis auth_authorize RPC and session reference headers",
            "migration": "remove_internal_only",
            "requirement": (
                "direct in-process typed calls in the merged monolith; no internal "
                "auth_authorize RPC or associated Redis result cache remains; retain the HTTP ingress check "
                "route, and add no alias for it "
                "without a separate reviewed contract"
            ),
        },
        {
            "id": "info.disclosure",
            "baseline": "/auth/info has no local authentication check and target absent returns raw authentication context JSON",
            "migration": "correct",
            "requirement": "classify consumers, authorize access, and return a typed redacted projection; do not mount the raw mapper contract by default",
        },
    ]


def _ui_contract(logout_consumer_sources: list[str]) -> dict[str, Any]:
    return {
        "api_auth_surface": {
            "api_slice_path": "/auth",
            "documented_api_base": "/api/v2/",
            "effective_prefix": "/api/v2/auth",
            "relationship_to_browser_auth": (
                "EliteaUI API-relative /auth requests are not a root browser /auth alias"
            ),
        },
        "logout_consumers": [
            "Admin UI sidebar assigns same-origin /auth/logout",
            "settings action assigns same-origin /auth/logout",
            "sidebar user action assigns same-origin /auth/logout",
        ],
        "logout_consumer_sources": logout_consumer_sources,
        "reauthentication": {
            "detection": "a fetch redirect URL containing both /auth/ and /login",
            "callback_route": "/auth-callback",
            "callback_query": "auth_state must survive the authentication return target unchanged",
            "success_channels": ["same-origin postMessage", "BroadcastChannel", "localStorage"],
            "retry": "retry the cloned original request after popup success",
        },
        "security_boundary": (
            "UI auth_state is popup correlation, not provider assertion validation or server-side Form/OIDC/SAML CSRF protection"
        ),
        "change_budget": (
            "preserving redirect paths and the callback query allows the first backend cutover with no intentional UI route-shape change"
        ),
    }


def _deployment_contract(
    centry_root: Path,
    auth_core_root: Path,
    auth_init_root: Path,
    runtime_interface_litellm_root: Path,
    elitea_core_root: Path,
) -> dict[str, Any]:
    auth_pylon_text = (centry_root / "pylon_auth/pylon.yml").read_text(encoding="utf-8")
    main_pylon_text = (centry_root / "pylon_main/pylon.yml").read_text(encoding="utf-8")
    runtime_services = _selected_runtime_services(
        (centry_root / "docker-compose.yml").read_text(encoding="utf-8")
    )
    auth_core = _effective_auth_core_config(auth_core_root, centry_root)
    defaults = _selected_default_env(
        (centry_root / "envs/default.env").read_text(encoding="utf-8")
    )
    auth_main = _selected_main_auth_config(
        (centry_root / "pylon_main/configs/auth.yml").read_text(encoding="utf-8")
    )
    litellm_public = _selected_litellm_public_config(
        (runtime_interface_litellm_root / "config.yml").read_text(encoding="utf-8")
    )
    elitea_core_public = _selected_elitea_core_public_config(
        (elitea_core_root / "config.yml").read_text(encoding="utf-8")
    )
    elitea_core_public_override = _selected_elitea_core_public_override(
        (centry_root / "pylon_main/configs/elitea_core.yml").read_text(encoding="utf-8")
    )
    if elitea_core_public_override["public_messages_route"] is not None:
        elitea_core_public = {
            "public_messages_route": elitea_core_public_override[
                "public_messages_route"
            ]
        }
    main_shared = _selected_main_shared_config(
        (centry_root / "pylon_main/configs/shared.yml").read_text(encoding="utf-8")
    )
    init_config = _effective_auth_init_config(auth_init_root, centry_root)
    form_contract = _safe_form_config(
        (centry_root / "pylon_auth/configs/auth_form.yml").read_text(encoding="utf-8")
    )
    auth_selected = _selected_auth_pylon(auth_pylon_text)
    main_selected = _selected_main_pylon(main_pylon_text)
    expected_auth_selected = {
        "application.APPLICATION_ROOT": "/auth/",
        "application.PERMANENT_SESSION_LIFETIME": "${COOKIES_LIFETIME}",
        "application.PREFERRED_URL_SCHEME": "${APP_PROTO}",
        "application.SERVER_NAME": "${APP_HOST}",
        "application.SESSION_COOKIE_DOMAIN": "${APP_HOST}",
        "application.SESSION_COOKIE_HTTPONLY": "true",
        "application.SESSION_COOKIE_NAME": "${NAME_PREFIX}_auth_session",
        "application.SESSION_COOKIE_PATH": "/",
        "application.SESSION_COOKIE_SAMESITE": None,
        "application.SESSION_COOKIE_SECURE": "${COOKIES_SECURE}",
        "application.SECRET_KEY_configured": True,
        "exposure.event_node.type": "RedisEventNode",
        "exposure.expose": "true",
        "rpc.redis_configured": True,
        "server.path": "/auth/",
        "server.proxy.x_for": "1",
        "server.proxy.x_host": "1",
        "server.proxy.x_proto": "1",
        "sessions.prefix": "${NAME_PREFIX}_auth_session_",
        "sessions.redis_configured": True,
    }
    if auth_selected != expected_auth_selected:
        raise ValueError("reviewed pylon_auth composition changed")
    if main_selected != {
        "exposure.event_node.type": "RedisEventNode",
        "exposure.handle.enabled": "true",
        "exposure.handle.prefixes": ["/auth"],
    }:
        raise ValueError("reviewed pylon_main alias changed")
    if auth_core.get("auth_provider") != "form" or auth_main.get("auth_mode") != "rpc":
        raise ValueError("effective tracked auth provider or pylon_main auth mode changed")
    if main_shared != {"settings.allow_cors": "true"}:
        raise ValueError("reviewed pylon_main CORS setting changed")
    if auth_core != {
        "allow_auth_traversal": True,
        "additional_headers": {"Server": "Centry"},
        "auth_denied_url": "${APP_PROTO}://${APP_HOST}/access_denied",
        "auth_provider": "form",
        "db_options.isolation_level": "AUTOCOMMIT",
        "default_login_url": "${APP_PROTO}://${APP_HOST}/",
        "default_logout_url": "${APP_PROTO}://${APP_HOST}/",
        "other_auth_headers": {},
        "register_error_handler": True,
        "url_prefix": "/",
    }:
        raise ValueError("reviewed effective Auth Core configuration changed")
    if init_config != {
        "initial_global_admins": ["admin"],
        "initial_root_permissions": [],
    }:
        raise ValueError("reviewed effective Auth Init configuration changed")
    if defaults != {
        "APP_PROTO": "http",
        "COOKIES_LIFETIME": "604800",
        "COOKIES_SECURE": "false",
        "NAME_PREFIX": "centry",
    }:
        raise ValueError("reviewed tracked environment defaults changed")
    if runtime_services != {
        "pylon_auth.image": "ghcr.io/eliteaai/pylon:1.2.25",
        "pylon_auth.web_runtime": "gevent",
        "pylon_main.image": "ghcr.io/eliteaai/pylon:1.2.25",
        "pylon_main.web_runtime": "gevent",
    }:
        raise ValueError("reviewed Pylon runtime composition changed")
    runtime_ref = runtime_services["pylon_auth.image"].rsplit(":", 1)[1]
    return {
        "auth_core": {
            "allow_auth_traversal": auth_core.get("allow_auth_traversal"),
            "additional_headers": auth_core.get("additional_headers"),
            "auth_denied_url": auth_core.get("auth_denied_url"),
            "db_isolation_level": auth_core.get("db_options.isolation_level"),
            "default_login_url": auth_core.get("default_login_url"),
            "default_logout_url": auth_core.get("default_logout_url"),
            "other_auth_headers": auth_core.get("other_auth_headers"),
            "provider": auth_core.get("auth_provider"),
            "register_error_handler": auth_core.get("register_error_handler"),
            "url_prefix": auth_core.get("url_prefix"),
        },
        "auth_form": form_contract,
        "auth_init": {
            "initial_global_admin_provider_references": init_config[
                "initial_global_admins"
            ],
            "initial_root_permissions": init_config["initial_root_permissions"],
        },
        "pylon_auth_selected": auth_selected,
        "pylon_main": {
            "allow_cors": True,
            "auth_mode": auth_main.get("auth_mode"),
            "browser_auth_exposure": main_selected,
            "public_uri_rules": auth_main.get("public_uri_rules"),
        },
        "public_rule_ownership": _main_public_rule_contract(
            auth_main,
            litellm_public,
            elitea_core_public,
        ),
        "runtime_composition": {
            **runtime_services,
            "pylon_source_ref": runtime_ref,
        },
        "tracked_environment_defaults": defaults,
        "runtime_override_limit": (
            "config-provider payloads and local override.env are intentionally excluded; production parity requires a redacted effective-config export"
        ),
    }


def build_catalog(
    auth_core_root: Path,
    auth_form_root: Path,
    auth_idp_rpc_root: Path,
    auth_init_root: Path,
    auth_mappers_root: Path,
    main_auth_root: Path,
    admin_ui_root: Path,
    centry_root: Path,
    pylon_root: Path,
    ui_root: Path,
) -> dict[str, Any]:
    roots = {
        "auth_core": auth_core_root.resolve(),
        "auth_form": auth_form_root.resolve(),
        "auth_idp_rpc": auth_idp_rpc_root.resolve(),
        "auth_init": auth_init_root.resolve(),
        "auth_mappers": auth_mappers_root.resolve(),
        "main_auth": main_auth_root.resolve(),
        "admin_ui": admin_ui_root.resolve(),
        "runtime_interface_litellm": (
            main_auth_root.resolve().parent / "runtime_interface_litellm"
        ),
        "artifacts": main_auth_root.resolve().parent / "artifacts",
        "elitea_core": main_auth_root.resolve().parent / "elitea_core",
        "centry": centry_root.resolve(),
        "pylon": pylon_root.resolve(),
        "ui": ui_root.resolve(),
    }
    files = {
        "auth_core": AUTH_CORE_FILES,
        "auth_form": AUTH_FORM_FILES,
        "auth_idp_rpc": AUTH_IDP_RPC_FILES,
        "auth_init": AUTH_INIT_FILES,
        "auth_mappers": AUTH_MAPPERS_FILES,
        "main_auth": MAIN_AUTH_FILES,
        "admin_ui": ADMIN_UI_FILES,
        "runtime_interface_litellm": RUNTIME_INTERFACE_LITELLM_FILES,
        "artifacts": ARTIFACTS_FILES,
        "elitea_core": ELITEA_CORE_FILES,
        "centry": CENTRY_FULL_FILES,
        "pylon": PYLON_FILES,
        "ui": UI_FILES,
    }
    repos = {label: _repo(root) for label, root in roots.items()}
    deployment_contract = _deployment_contract(
        roots["centry"],
        roots["auth_core"],
        roots["auth_init"],
        roots["runtime_interface_litellm"],
        roots["elitea_core"],
    )
    pylon_ref = deployment_contract["runtime_composition"]["pylon_source_ref"]
    pylon_version = _ref_bytes(repos["pylon"], pylon_ref, "version.txt").decode(
        "utf-8"
    ).strip()
    if pylon_version != pylon_ref:
        raise ValueError(
            f"Pylon runtime image tag {pylon_ref!r} does not match version.txt {pylon_version!r}"
        )
    source_hashes = _source_hashes(
        roots,
        {label: relative for label, relative in files.items() if label != "pylon"},
    )
    source_hashes.update(
        _ref_source_hashes("pylon", repos["pylon"], pylon_ref, PYLON_FILES)
    )
    route_composition = _route_composition(deployment_contract)
    framework_contract = _framework_contract(
        roots["auth_core"], repos["pylon"], pylon_ref
    )
    logout_consumer_sources = sorted(
        [
            f"ui/{path}"
            for path in _tracked_literal_paths(
                repos["ui"], "/auth/logout", ("src/",)
            )
        ]
        + [
            f"admin_ui/{path}"
            for path in _tracked_literal_paths(
                repos["admin_ui"], "/auth/logout", ("frontend/src/",)
            )
        ]
    )
    expected_logout_sources = [
        "admin_ui/frontend/src/components/Layout/Sidebar.jsx",
        "ui/src/[fsd]/pages/settings/index.jsx",
        "ui/src/[fsd]/widgets/sidebar-root/ui/button/UserButton.jsx",
    ]
    if logout_consumer_sources != expected_logout_sources:
        raise ValueError(
            "reviewed direct /auth/logout consumer set changed: "
            f"{logout_consumer_sources!r}"
        )

    trees: dict[tuple[str, str], ast.Module] = {}
    for label, targets_by_file in FINGERPRINT_TARGETS.items():
        for relative in targets_by_file:
            trees[(label, relative)] = (
                _parse_ref(repos["pylon"], pylon_ref, relative)
                if label == "pylon"
                else _parse(roots[label] / relative)
            )

    fingerprints: dict[str, dict[str, Any]] = {}
    for label, targets_by_file in sorted(FINGERPRINT_TARGETS.items()):
        for relative, targets in sorted(targets_by_file.items()):
            tree = trees[(label, relative)]
            for class_name, method_name in targets:
                key = f"{label}/{relative}#{class_name}.{method_name}"
                fingerprints[key] = _fingerprint(_method(tree, class_name, method_name))
    session_tree = trees[("pylon", "pylon/core/tools/session.py")]
    for function_name in ("_destroy", "_regenerate", "make_session_interface"):
        key = f"pylon/pylon/core/tools/session.py#{function_name}"
        fingerprints[key] = _fingerprint(_function(session_tree, function_name))
    exposure_tree = _parse_ref(
        repos["pylon"], pylon_ref, "pylon/core/tools/exposure.py"
    )
    for function_name in (
        "expose",
        "on_pylon_exposed",
        "on_request",
        "prepare_call_environ",
        "prepare_rpc_environ",
        "wsgi_call",
    ):
        key = f"pylon/pylon/core/tools/exposure.py#{function_name}"
        fingerprints[key] = _fingerprint(_function(exposure_tree, function_name))
    server_init_tree = _parse_ref(
        repos["pylon"], pylon_ref, "pylon/core/tools/server/init.py"
    )
    fingerprints["pylon/pylon/core/tools/server/init.py#init_context"] = _fingerprint(
        _function(server_init_tree, "init_context")
    )
    web_tree = _parse_ref(repos["pylon"], pylon_ref, "pylon/core/tools/web.py")
    fingerprints["pylon/pylon/core/tools/web.py#route"] = _fingerprint(
        _function(web_tree, "route")
    )
    main_tree = _parse_ref(repos["pylon"], pylon_ref, "pylon/main.py")
    fingerprints["pylon/pylon/main.py#main"] = _fingerprint(
        _function(main_tree, "main")
    )
    gevent_tree = _parse_ref(
        repos["pylon"], pylon_ref, "pylon/core/tools/server/gevent.py"
    )
    for function_name in ("make_server", "run_server"):
        key = f"pylon/pylon/core/tools/server/gevent.py#{function_name}"
        fingerprints[key] = _fingerprint(_function(gevent_tree, function_name))
    router_init_tree = _parse_ref(
        repos["pylon"], pylon_ref, "pylon/framework/router/init.py"
    )
    fingerprints["pylon/pylon/framework/router/init.py#init"] = _fingerprint(
        _function(router_init_tree, "init")
    )

    route_contracts: dict[str, dict[str, Any]] = {}
    for key, expected in sorted(EXPECTED_ROUTES.items()):
        source, symbol = key.split("#", 1)
        label, relative = source.split("/", 1)
        class_name, method_name = symbol.split(".", 1)
        observed = _route_spec(_method(trees[(label, relative)], class_name, method_name))
        if observed != expected:
            raise ValueError(f"reviewed route changed for {key}: {observed!r}")
        declared = observed[1]
        exposed = set(declared) | {"OPTIONS"}
        if "GET" in declared:
            exposed.add("HEAD")
        blueprint_prefix = (
            route_composition["auth_core_blueprint_prefix"]
            if label == "auth_core"
            else route_composition["auth_form_blueprint_prefix"]
        )
        route_contracts[key] = {
            "blueprint_prefix": blueprint_prefix,
            "declared_methods": declared,
            "effective_route": _join_url_path(
                route_composition["external_base_path"],
                blueprint_prefix,
                observed[0],
            ),
            "inner_flask_methods": sorted(exposed),
            "source_route": observed[0],
        }

    provenance = {
        "auth_core_repo": _provenance(
            repos["auth_core"], "pylon_auth/plugins/auth_core", set(AUTH_CORE_FILES)
        ),
        "auth_form_repo": _provenance(
            repos["auth_form"], "pylon_auth/plugins/auth_form", set(AUTH_FORM_FILES)
        ),
        "auth_idp_rpc_repo": _provenance(
            repos["auth_idp_rpc"],
            "pylon_auth/plugins/auth_idp_rpc",
            set(AUTH_IDP_RPC_FILES),
        ),
        "auth_init_repo": _provenance(
            repos["auth_init"], "pylon_auth/plugins/auth_init", set(AUTH_INIT_FILES)
        ),
        "auth_mappers_repo": _provenance(
            repos["auth_mappers"],
            "pylon_auth/plugins/auth_mappers",
            set(AUTH_MAPPERS_FILES),
        ),
        "main_auth_repo": _provenance(
            repos["main_auth"], "pylon_main/plugins/auth", set(MAIN_AUTH_FILES)
        ),
        "runtime_interface_litellm_repo": _provenance(
            repos["runtime_interface_litellm"],
            "pylon_main/plugins/runtime_interface_litellm",
            set(RUNTIME_INTERFACE_LITELLM_FILES),
        ),
        "artifacts_repo": _provenance(
            repos["artifacts"],
            "pylon_main/plugins/artifacts",
            set(ARTIFACTS_FILES),
        ),
        "elitea_core_repo": _provenance(
            repos["elitea_core"],
            "pylon_main/plugins/elitea_core",
            set(ELITEA_CORE_FILES),
        ),
        "admin_ui_repo": _provenance(
            repos["admin_ui"], "pylon_main/plugins/admin_ui", set(ADMIN_UI_FILES)
        ),
        "centry_repo": _provenance(
            repos["centry"],
            ".",
            set(CENTRY_FULL_FILES),
            {
                "docker-compose.yml": _selected_runtime_services,
                "envs/default.env": _selected_default_env,
                "pylon_auth/configs/auth_core.yml": _selected_auth_core_config,
                "pylon_auth/configs/auth_form.yml": _safe_form_config,
                "pylon_auth/configs/auth_init.yml": _selected_auth_init_config,
                "pylon_auth/pylon.yml": _selected_auth_pylon,
                "pylon_main/configs/auth.yml": _selected_main_auth_config,
                "pylon_main/configs/elitea_core.yml": _selected_elitea_core_public_override,
                "pylon_main/configs/shared.yml": _selected_main_shared_config,
                "pylon_main/pylon.yml": _selected_main_pylon,
            },
        ),
        "pylon_repo": _provenance_at_ref(repos["pylon"], ".", pylon_ref),
        "ui_repo": _provenance(repos["ui"], ".", set(UI_FILES)),
    }

    source_inventory = {
        "full_byte_sources": sorted(source_hashes),
        "optional_runtime_configs": _optional_runtime_config_inventory(
            roots["centry"]
        ),
        "tracked_auth_mapper_contract": _tracked_auth_mapper_contract(
            roots["auth_mappers"]
        ),
        "selected_config_sources": [
            "centry/docker-compose.yml#auth_runtime_allowlist",
            "centry/envs/default.env#auth_nonsecret_allowlist",
            "centry/pylon_auth/configs/auth_core.yml#browser_auth_allowlist",
            "centry/pylon_auth/configs/auth_form.yml#redacted_form_shape",
            "centry/pylon_auth/configs/auth_init.yml#initial_admin_allowlist",
            "centry/pylon_auth/pylon.yml#auth_http_and_session_allowlist",
            "centry/pylon_main/configs/auth.yml#auth_gate_allowlist",
            "centry/pylon_main/configs/elitea_core.yml#public_route_override_allowlist",
            "centry/pylon_main/configs/shared.yml#cors_allowlist",
            "centry/pylon_main/pylon.yml#browser_auth_exposure_allowlist",
            "elitea_core/config.yml#public_route_allowlist",
            "runtime_interface_litellm/config.yml#public_route_allowlist",
        ],
        "selected_config_sha256": {
            "centry/docker-compose.yml#auth_runtime_allowlist": _canonical_sha256(
                _selected_runtime_services(
                    (roots["centry"] / "docker-compose.yml").read_text(encoding="utf-8")
                )
            ),
            "centry/envs/default.env#auth_nonsecret_allowlist": _canonical_sha256(
                _selected_default_env(
                    (roots["centry"] / "envs/default.env").read_text(encoding="utf-8")
                )
            ),
            "centry/pylon_auth/configs/auth_core.yml#browser_auth_allowlist": _canonical_sha256(
                _selected_auth_core_config(
                    (roots["centry"] / "pylon_auth/configs/auth_core.yml").read_text(
                        encoding="utf-8"
                    )
                )
            ),
            "centry/pylon_auth/configs/auth_form.yml#redacted_form_shape": _canonical_sha256(
                _safe_form_config(
                    (roots["centry"] / "pylon_auth/configs/auth_form.yml").read_text(
                        encoding="utf-8"
                    )
                )
            ),
            "centry/pylon_auth/configs/auth_init.yml#initial_admin_allowlist": _canonical_sha256(
                _selected_auth_init_config(
                    (roots["centry"] / "pylon_auth/configs/auth_init.yml").read_text(
                        encoding="utf-8"
                    )
                )
            ),
            "centry/pylon_auth/pylon.yml#auth_http_and_session_allowlist": _canonical_sha256(
                _selected_auth_pylon(
                    (roots["centry"] / "pylon_auth/pylon.yml").read_text(encoding="utf-8")
                )
            ),
            "centry/pylon_main/configs/auth.yml#auth_gate_allowlist": _canonical_sha256(
                _selected_main_auth_config(
                    (roots["centry"] / "pylon_main/configs/auth.yml").read_text(
                        encoding="utf-8"
                    )
                )
            ),
            "centry/pylon_main/configs/elitea_core.yml#public_route_override_allowlist": _canonical_sha256(
                _selected_elitea_core_public_override(
                    (roots["centry"] / "pylon_main/configs/elitea_core.yml").read_text(
                        encoding="utf-8"
                    )
                )
            ),
            "centry/pylon_main/configs/shared.yml#cors_allowlist": _canonical_sha256(
                _selected_main_shared_config(
                    (roots["centry"] / "pylon_main/configs/shared.yml").read_text(
                        encoding="utf-8"
                    )
                )
            ),
            "centry/pylon_main/pylon.yml#browser_auth_exposure_allowlist": _canonical_sha256(
                _selected_main_pylon(
                    (roots["centry"] / "pylon_main/pylon.yml").read_text(encoding="utf-8")
                )
            ),
            "elitea_core/config.yml#public_route_allowlist": _canonical_sha256(
                _selected_elitea_core_public_config(
                    (roots["elitea_core"] / "config.yml").read_text(encoding="utf-8")
                )
            ),
            "runtime_interface_litellm/config.yml#public_route_allowlist": _canonical_sha256(
                _selected_litellm_public_config(
                    (roots["runtime_interface_litellm"] / "config.yml").read_text(
                        encoding="utf-8"
                    )
                )
            ),
        },
    }

    return {
        "behavior_contracts": _behavior_contracts(),
        "behavior_fingerprints": fingerprints,
        "deployment_contract": deployment_contract,
        "effective_routes": route_contracts,
        "framework_contract": framework_contract,
        "http_outcomes": _http_outcomes(deployment_contract, route_composition),
        "inference_limits": [
            "This is not a complete pylon_auth, pylon_main Auth, administrative API, OIDC, SAML, IdP RPC, or RBAC parity claim.",
            "Tracked configuration can be overridden by the runtime config provider; no running environment or secret source is inspected.",
            "Pylon and browser behavior is derived from pinned source and configuration, not from a live Flask/Traefik execution.",
            "The Centry image tag is bound to the same local Pylon Git tag, but the deployed container image digest was not pulled or live-verified.",
            "The pylon.yml files are represented only by the named auth/session/exposure selectors; unrelated server settings are outside this contract.",
            "The reviewed direct Auth Core public-rule set is empty; tracked Main-local configured and dynamic registrations are inventoried separately, while runtime config-provider payloads or unreviewed plugins can still change the effective Main set.",
            "Migration dispositions preserve valid-client behavior but intentionally correct malformed or ambiguous inputs; they are not a byte-for-byte HTTP parity claim and require cutover validation.",
            "UI source is pinned only for browser-login redirects, logout, popup callback, and request retry behavior.",
        ],
        "provenance": provenance,
        "route_composition": route_composition,
        "schema_version": SCHEMA_VERSION,
        "scope": {
            "current_root_auth_alias": False,
            "full_auth_parity_claim": False,
            "includes": [
                "Auth Core browser routes and redirect/session orchestration",
                "Form login/logout provider",
                "pylon_main browser authorization RPC divergence",
                "Auth Init login processor business rules",
                "Auth mapper info behavior",
                "Pylon server-side session/cookie mechanics",
                "tracked Centry browser-auth composition",
                "tracked Main-local configured and dynamic public-rule registrations",
                "EliteaUI browser-auth consumers",
                "Admin UI logout consumer",
            ],
            "migration_baseline_name": "current implementation",
        },
        "security_dispositions": _security_dispositions(),
        "source_files_sha256": source_hashes,
        "source_inventory": source_inventory,
        "ui_contract": _ui_contract(logout_consumer_sources),
    }


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--auth-core-root", type=Path, required=True)
    parser.add_argument("--auth-form-root", type=Path, required=True)
    parser.add_argument("--auth-idp-rpc-root", type=Path, required=True)
    parser.add_argument("--auth-init-root", type=Path, required=True)
    parser.add_argument("--auth-mappers-root", type=Path, required=True)
    parser.add_argument("--main-auth-root", type=Path, required=True)
    parser.add_argument("--admin-ui-root", type=Path, required=True)
    parser.add_argument("--centry-root", type=Path, required=True)
    parser.add_argument("--pylon-root", type=Path, required=True)
    parser.add_argument("--ui-root", type=Path, required=True)
    parser.add_argument(
        "--output",
        type=Path,
        default=Path("testdata/baseline/browser-auth-http-contracts.json"),
    )
    parser.add_argument("--check", action="store_true")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    try:
        catalog = build_catalog(
            args.auth_core_root,
            args.auth_form_root,
            args.auth_idp_rpc_root,
            args.auth_init_root,
            args.auth_mappers_root,
            args.main_auth_root,
            args.admin_ui_root,
            args.centry_root,
            args.pylon_root,
            args.ui_root,
        )
    except ValueError as exc:
        print(f"browser auth evidence export failed: {exc}", file=sys.stderr)
        return 1
    rendered = json.dumps(catalog, indent=2, sort_keys=True) + "\n"
    if args.check:
        if not args.output.is_file():
            print(f"missing checked browser auth evidence: {args.output}", file=sys.stderr)
            return 1
        if args.output.read_text(encoding="utf-8") != rendered:
            print(
                "browser auth current-baseline evidence differs from reviewed fixture; "
                "run the refresh task and review the diff",
                file=sys.stderr,
            )
            return 1
        return 0
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(rendered, encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
