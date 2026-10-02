#!/usr/bin/env python3
"""Export bounded OIDC and SAML current-baseline authentication evidence.

The exporter is deliberately dependency-free: it parses only the small YAML
surface selected below and fingerprints Python behavior through the standard
library AST.  It does not import or execute Pylon, Flask, provider libraries,
configuration substitution, network calls, or database code.

Credential, certificate, private-key, and endpoint values are never emitted or
hashed.  Their presence state is evidence; rotation of a secret is not.  This
fixture describes checked-in source/configuration only and must not be read as
proof that either federated provider is configured or deployed.
"""

from __future__ import annotations

import argparse
import ast
import hashlib
import json
import os
import re
import subprocess
import sys
from pathlib import Path
from typing import Any, Callable


SCHEMA_VERSION = 1

HTTP_METHOD_ORDER = ("DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT")

ROOT_SOURCE_FILES: dict[str, tuple[str, ...]] = {
    "auth_oidc": (
        "admin_schema.json",
        "methods/jwt.py",
        "methods/tools.py",
        "module.py",
        "requirements.txt",
        "routes/login.py",
        "routes/logout.py",
        "templates/redirect.html",
    ),
    "auth_saml": (
        "methods/tools.py",
        "module.py",
        "requirements.txt",
        "routes/login.py",
        "routes/logout.py",
        "templates/redirect.html",
    ),
    "auth_core": (
        "admin_schema.json",
        "db/migrations/202202021633_core.py",
        "methods/auth_context.py",
        "methods/hooks.py",
        "methods/redirects.py",
        "methods/urls.py",
        "routes/auth.py",
        "rpc/success_mappers.py",
        "rpc/user_providers.py",
    ),
    "auth_init": (
        "module.py",
        "rpc/processor.py",
    ),
    "auth_mappers": (
        "module.py",
        "rpc/header.py",
        "rpc/json.py",
        "rpc/noop.py",
    ),
    "main_auth": (
        "api/v2/user.py",
        "module.py",
    ),
    "social": (
        "api/v2/author.py",
        "events/social.py",
    ),
    "elitea_core": ("events/collection_config.py",),
    "bootstrap": (
        "events/runtime.py",
        "tools/event.py",
    ),
    "admin": (
        "api/v2/runtime_remote.py",
        "api/v2/runtime_remote_config.py",
    ),
    "pylon": (
        "pylon/core/tools/exposure.py",
        "pylon/core/tools/module/descriptor.py",
        "pylon/core/tools/session.py",
        "requirements.txt",
    ),
    "ui": (
        "src/[fsd]/features/auth/lib/helpers/authPopup.helpers.js",
        "src/[fsd]/pages/settings/index.jsx",
        "src/[fsd]/widgets/sidebar-root/ui/button/UserButton.jsx",
        "src/api/eliteaApi.js",
        "src/routes.js",
    ),
}

# Config files are selected semantically and hashed only after secret-bearing
# values have been reduced to safe presence states.
ROOT_CONFIG_FILES: dict[str, tuple[str, ...]] = {
    "auth_oidc": ("config.yml",),
    "auth_saml": ("config.yml",),
    "auth_core": ("config.yml",),
    "auth_init": ("config.yml",),
    "auth_mappers": ("config.yml",),
}

RUNTIME_CONFIG_FILES = (
    "pylon_auth/configs/auth_core.yml",
    "pylon_auth/configs/auth_init.yml",
    "pylon_auth/configs/auth_oidc.yml",
    "pylon_auth/configs/bootstrap.yml",
    "pylon_auth/pylon.yml",
    "pylon_main/configs/auth.yml",
    "pylon_main/pylon.yml",
)

FINGERPRINT_TARGETS: dict[str, tuple[tuple[str, str, str], ...]] = {
    "auth_oidc": (
        ("methods/jwt.py", "Method", "_init"),
        ("methods/tools.py", "Method", "generate_state_id"),
        ("methods/tools.py", "Method", "get_metadata"),
        ("methods/tools.py", "Method", "get_state_id"),
        ("module.py", "Module", "init"),
        ("module.py", "Module", "reconfig"),
        ("routes/login.py", "Route", "login"),
        ("routes/login.py", "Route", "login_callback"),
        ("routes/logout.py", "Route", "logout"),
        ("routes/logout.py", "Route", "logout_callback"),
    ),
    "auth_saml": (
        ("methods/tools.py", "Method", "data_to_xml_tree"),
        ("methods/tools.py", "Method", "xml_tree_to_json"),
        ("module.py", "Module", "init"),
        ("routes/login.py", "Route", "acs"),
        ("routes/login.py", "Route", "login"),
        ("routes/logout.py", "Route", "logout"),
        ("routes/logout.py", "Route", "sls"),
    ),
    "auth_core": (
        ("methods/auth_context.py", "Method", "get_auth_context"),
        ("methods/auth_context.py", "Method", "set_auth_context"),
        ("methods/hooks.py", "Method", "error_handler"),
        ("methods/redirects.py", "Method", "access_needed_redirect"),
        ("methods/redirects.py", "Method", "access_success_redirect"),
        ("methods/redirects.py", "Method", "logout_needed_redirect"),
        ("methods/redirects.py", "Method", "logout_success_redirect"),
        ("methods/urls.py", "Method", "get_relative_url_prefix"),
        ("routes/auth.py", "Route", "auth"),
        ("rpc/success_mappers.py", "RPC", "rpc_success_mapper"),
        ("rpc/user_providers.py", "RPC", "get_user_from_provider"),
    ),
    "auth_init": (
        ("module.py", "Module", "init"),
        ("rpc/processor.py", "RPC", "init_auth_processor"),
    ),
    "auth_mappers": (
        ("module.py", "Module", "init"),
        ("rpc/header.py", "RPC", "header_success_mapper"),
        ("rpc/json.py", "RPC", "json_info_mapper"),
        ("rpc/json.py", "RPC", "json_success_mapper"),
    ),
    "main_auth": (
        ("api/v2/user.py", "API", "get"),
        ("module.py", "Module", "_before_request_hook"),
    ),
    "social": (
        ("api/v2/author.py", "ProjectApi", "get"),
        ("events/social.py", "Event", "add_social_user_data"),
    ),
    "elitea_core": (("events/collection_config.py", "Event", "handle_new_ai_user"),),
    "bootstrap": (
        ("events/runtime.py", "Event", "_bootstrap_runtime_update"),
        ("tools/event.py", "RuntimeAnnoucer", "_collect_info"),
        ("tools/event.py", "RuntimeAnnoucer", "run"),
    ),
    "admin": (
        ("api/v2/runtime_remote.py", "AdminAPI", "post"),
        ("api/v2/runtime_remote_config.py", "AdminAPI", "get"),
    ),
    "pylon": (
        ("pylon/core/tools/module/descriptor.py", "ModuleDescriptor", "load_config"),
    ),
}

ROUTE_TARGETS: dict[str, tuple[tuple[str, str, str, str], ...]] = {
    "auth_oidc": (
        ("routes/login.py", "Route", "login", "oidc.login"),
        ("routes/login.py", "Route", "login_callback", "oidc.callback"),
        ("routes/logout.py", "Route", "logout", "oidc.logout"),
        ("routes/logout.py", "Route", "logout_callback", "oidc.logout_callback"),
    ),
    "auth_saml": (
        ("routes/login.py", "Route", "login", "saml.login"),
        ("routes/login.py", "Route", "acs", "saml.acs"),
        ("routes/logout.py", "Route", "logout", "saml.logout"),
        ("routes/logout.py", "Route", "sls", "saml.sls"),
    ),
}

SECRET_BEARING_CONFIG_PATHS = {
    "auth_oidc/config.yml",
    "auth_saml/config.yml",
    "centry/pylon_auth/configs/auth_oidc.yml",
}


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _canonical_sha256(value: Any) -> str:
    return _sha256(
        json.dumps(value, sort_keys=True, separators=(",", ":")).encode("utf-8")
    )


def _file_sha256(path: Path) -> str:
    return _sha256(path.read_bytes())


def _run_git(repo: Path, *args: str, binary: bool = False) -> bytes | str:
    result = subprocess.run(
        ["git", "-C", str(repo), *args],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=not binary,
    )
    if result.returncode != 0:
        stderr = result.stderr.decode() if binary else result.stderr
        raise ValueError(f"git {' '.join(args)} failed for {repo}: {stderr.strip()}")
    return result.stdout


def _nul_paths(raw: bytes) -> set[str]:
    return {
        os.fsdecode(value)
        for value in raw.split(b"\0")
        if value and not Path(os.fsdecode(value)).is_absolute()
    }


def _dirty_paths(repo: Path) -> set[str]:
    changed: set[str] = set()
    for options in ((), ("--cached",)):
        changed.update(
            _nul_paths(
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
    changed.update(
        _nul_paths(
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
    return changed


def git_provenance(repo: Path, contract_paths: set[str]) -> dict[str, Any]:
    dirty = _dirty_paths(repo)
    contract_dirty = sorted(dirty.intersection(contract_paths))
    return {
        "contract_source_dirty_paths": contract_dirty,
        "contract_sources_reconstructable_from_pinned_head": not contract_dirty,
        "pinned_head": str(_run_git(repo, "rev-parse", "HEAD")).strip(),
    }


def _parse(path: Path) -> ast.Module:
    try:
        return ast.parse(path.read_text(encoding="utf-8"), filename=path.as_posix())
    except (SyntaxError, UnicodeDecodeError) as exc:
        raise ValueError(f"cannot parse {path}: {exc}") from exc


def _method(tree: ast.Module, class_name: str, method_name: str) -> ast.FunctionDef:
    for node in tree.body:
        if not isinstance(node, ast.ClassDef) or node.name != class_name:
            continue
        for child in node.body:
            if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef)) and child.name == method_name:
                return child
    raise ValueError(f"missing method {class_name}.{method_name}")


def _fingerprint(node: ast.AST, source: str, class_name: str, method_name: str) -> dict[str, Any]:
    payload = ast.dump(node, annotate_fields=True, include_attributes=False)
    return {
        "ast_sha256": _sha256(payload.encode("utf-8")),
        "class": class_name,
        "function": method_name,
        "line": getattr(node, "lineno", None),
        "source": source,
    }


def _route_spec(node: ast.FunctionDef) -> tuple[str, list[str]]:
    for decorator in node.decorator_list:
        if not isinstance(decorator, ast.Call):
            continue
        function = decorator.func
        if not isinstance(function, ast.Attribute) or function.attr != "route":
            continue
        if not decorator.args or not isinstance(decorator.args[0], ast.Constant) or not isinstance(decorator.args[0].value, str):
            raise ValueError("route path must be a literal string")
        methods = ["GET"]
        for keyword in decorator.keywords:
            if keyword.arg != "methods":
                continue
            if not isinstance(keyword.value, (ast.List, ast.Tuple)):
                raise ValueError("route methods must be a literal list")
            methods = []
            for item in keyword.value.elts:
                if not isinstance(item, ast.Constant) or not isinstance(item.value, str):
                    raise ValueError("route method must be a literal string")
                methods.append(item.value.upper())
        return decorator.args[0].value, methods
    raise ValueError("method has no @web.route decorator")


def _effective_methods(declared: list[str]) -> list[str]:
    methods = set(declared)
    if "GET" in methods:
        methods.add("HEAD")
    methods.add("OPTIONS")
    return [method for method in HTTP_METHOD_ORDER if method in methods]


def _strip_yaml_comment(value: str) -> str:
    quote: str | None = None
    escaped = False
    result: list[str] = []
    for character in value:
        if escaped:
            result.append(character)
            escaped = False
            continue
        if character == "\\" and quote == '"':
            result.append(character)
            escaped = True
            continue
        if character in {"'", '"'}:
            if quote is None:
                quote = character
            elif quote == character:
                quote = None
            result.append(character)
            continue
        if character == "#" and quote is None:
            break
        result.append(character)
    return "".join(result).rstrip()


def _yaml_scalar(raw: str) -> Any:
    value = raw.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in {"'", '"'}:
        return value[1:-1]
    lowered = value.lower()
    if lowered in {"true", "false"}:
        return lowered == "true"
    if lowered in {"null", "none", "~"}:
        return None
    if re.fullmatch(r"-?[0-9]+", value):
        return int(value)
    return value


def _yaml_paths(text: str) -> tuple[dict[str, Any], dict[str, list[Any]]]:
    scalars: dict[str, Any] = {}
    sequences: dict[str, list[Any]] = {}
    stack: list[tuple[int, str]] = []
    for raw_line in text.splitlines():
        without_comment = _strip_yaml_comment(raw_line)
        if not without_comment.strip():
            continue
        indent = len(without_comment) - len(without_comment.lstrip(" "))
        content = without_comment.strip()
        if content.startswith("- "):
            # YAML permits an indentationless sequence immediately below a
            # mapping key. Keep a key at the same indentation in that case.
            while stack and indent < stack[-1][0]:
                stack.pop()
            if not stack:
                raise ValueError("top-level YAML sequence is outside selected grammar")
            path = ".".join(item[1] for item in stack)
            sequences.setdefault(path, []).append(_yaml_scalar(content[2:]))
            continue
        while stack and indent <= stack[-1][0]:
            stack.pop()
        match = re.fullmatch(r"([^:]+):(.*)", content)
        if match is None:
            raise ValueError("unsupported YAML syntax in selected configuration")
        key = match.group(1).strip()
        value = match.group(2).strip()
        path_parts = [item[1] for item in stack] + [key]
        path = ".".join(path_parts)
        if value:
            scalars[path] = _yaml_scalar(value)
        else:
            stack.append((indent, key))
    return scalars, sequences


def _value_state(values: dict[str, Any], path: str) -> str:
    if path not in values:
        return "absent"
    value = values[path]
    if value in {None, ""}:
        return "empty"
    if isinstance(value, str) and "${" in value:
        return "environment_template_present_but_redacted"
    return "inline_value_present_but_redacted"


def _safe_oidc_base(text: str) -> dict[str, Any]:
    values, _ = _yaml_paths(text)
    fields = [
        "authorization_endpoint",
        "token_endpoint",
        "userinfo_endpoint",
        "end_session_endpoint",
        "client_id",
        "client_secret",
        "issuer",
        "jwt_public_key",
        "url_prefix",
    ]
    return {
        "fields": {field: _value_state(values, field) for field in fields},
        "credential_or_endpoint_values_exported": False,
    }


def _safe_oidc_runtime(text: str) -> dict[str, Any]:
    values, _ = _yaml_paths(text)
    mode = values.get("url_mode")
    return {
        "fields": {
            field: _value_state(values, field)
            for field in ("metadata_endpoint", "client_id", "client_secret")
        },
        "url_mode": mode if mode in {"default", "external", "request"} else "other_or_absent",
        "credential_or_endpoint_values_exported": False,
    }


def _safe_saml_base(text: str) -> dict[str, Any]:
    values, _ = _yaml_paths(text)
    fields = [
        "sp_key",
        "sp_cert",
        "idp_cert",
        "idp_metadata",
        "authn_destination",
        "logout_destination",
        "saml_issuer",
        "url_prefix",
    ]
    return {
        "fields": {field: _value_state(values, field) for field in fields},
        "credential_certificate_or_endpoint_values_exported": False,
    }


def _safe_auth_core_base(text: str) -> dict[str, Any]:
    values, _ = _yaml_paths(text)
    return {
        "url_prefix": values.get("url_prefix"),
        "register_error_handler": values.get("register_error_handler", "code_default_true"),
        "traceback_error_handler": values.get("traceback_error_handler", "code_default_true"),
    }


def _safe_auth_init_base(text: str) -> dict[str, Any]:
    values, sequences = _yaml_paths(text)
    return {
        "initial_global_admins_count": len(sequences.get("initial_global_admins", [])),
        "initial_root_permissions_count": len(sequences.get("initial_root_permissions", [])),
        "top_level_scalar_keys": sorted(values),
    }


def _safe_auth_mappers_base(text: str) -> dict[str, Any]:
    values, _ = _yaml_paths(text)
    return {
        "configured_header_scopes": sorted(
            {
                path.split(".")[2]
                for path in values
                if path.startswith("header.scopes.") and len(path.split(".")) >= 4
            }
        ),
        "configured_json_scopes": sorted(
            {
                path.split(".")[2]
                for path in values
                if path.startswith("json.scopes.") and len(path.split(".")) >= 4
            }
        ),
        "projection_values_exported": False,
    }


def _safe_runtime_auth_core(text: str) -> dict[str, Any]:
    values, _ = _yaml_paths(text)
    provider = values.get("auth_provider")
    return {
        "auth_provider": provider if provider in {"form", "oidc", "saml"} else "other_or_absent",
        "auth_denied_url": _value_state(values, "auth_denied_url"),
        "default_login_url": _value_state(values, "default_login_url"),
        "default_logout_url": _value_state(values, "default_logout_url"),
        "url_values_exported": False,
    }


def _safe_runtime_auth_init(text: str) -> dict[str, Any]:
    _, sequences = _yaml_paths(text)
    admins = sequences.get("initial_global_admins", [])
    return {
        "initial_global_admins": admins,
        "matching": "exact provider-reference string equality",
    }


def _safe_bootstrap(text: str) -> dict[str, Any]:
    _, sequences = _yaml_paths(text)
    return {"preordered_plugins": sequences.get("preordered_plugins", [])}


def _safe_auth_pylon(text: str) -> dict[str, Any]:
    values, _ = _yaml_paths(text)
    return {
        "application_root": values.get("application.APPLICATION_ROOT"),
        "config_provider_path": values.get("modules.config.provider.path"),
        "server_path": values.get("server.path"),
        "session_cookie_httponly": values.get("application.SESSION_COOKIE_HTTPONLY"),
        "session_cookie_name": _value_state(values, "application.SESSION_COOKIE_NAME"),
        "session_cookie_path": values.get("application.SESSION_COOKIE_PATH"),
        "session_cookie_secure": _value_state(values, "application.SESSION_COOKIE_SECURE"),
        "session_lifetime": _value_state(values, "application.PERMANENT_SESSION_LIFETIME"),
        "session_prefix": _value_state(values, "sessions.prefix"),
        "secret_key": _value_state(values, "application.SECRET_KEY"),
        "secret_or_cookie_values_exported": False,
    }


def _safe_main_auth(text: str) -> dict[str, Any]:
    values, sequences = _yaml_paths(text)
    public_rules: list[str] = []
    for item in sequences.get("public_rules", []):
        if not isinstance(item, str):
            continue
        match = re.fullmatch(r"uri:\s*(['\"]?)(.+?)\1", item)
        if match is not None:
            public_rules.append(match.group(2))
    return {
        "auth_mode": values.get("auth_mode"),
        "configured_public_uri_rules": public_rules,
    }


def _safe_main_pylon(text: str) -> dict[str, Any]:
    values, sequences = _yaml_paths(text)
    return {
        "browser_auth_exposure_enabled": values.get("exposure.handle.enabled"),
        "browser_auth_exposure_prefixes": sequences.get("exposure.handle.prefixes", []),
    }


def _safe_config_selector(label: str) -> Callable[[str], dict[str, Any]]:
    selectors: dict[str, Callable[[str], dict[str, Any]]] = {
        "auth_oidc/config.yml": _safe_oidc_base,
        "auth_saml/config.yml": _safe_saml_base,
        "auth_core/config.yml": _safe_auth_core_base,
        "auth_init/config.yml": _safe_auth_init_base,
        "auth_mappers/config.yml": _safe_auth_mappers_base,
        "centry/pylon_auth/configs/auth_core.yml": _safe_runtime_auth_core,
        "centry/pylon_auth/configs/auth_init.yml": _safe_runtime_auth_init,
        "centry/pylon_auth/configs/auth_oidc.yml": _safe_oidc_runtime,
        "centry/pylon_auth/configs/bootstrap.yml": _safe_bootstrap,
        "centry/pylon_auth/pylon.yml": _safe_auth_pylon,
        "centry/pylon_main/configs/auth.yml": _safe_main_auth,
        "centry/pylon_main/pylon.yml": _safe_main_pylon,
    }
    try:
        return selectors[label]
    except KeyError as exc:
        raise ValueError(f"no safe configuration selector for {label}") from exc


def _selected_runtime_provenance(runtime_root: Path) -> dict[str, Any]:
    dirty = _dirty_paths(runtime_root)
    paths: dict[str, dict[str, Any]] = {}
    all_match = True
    for relative in RUNTIME_CONFIG_FILES:
        label = "centry/" + relative
        selector = _safe_config_selector(label)
        current = selector((runtime_root / relative).read_text(encoding="utf-8"))
        pinned_text = str(_run_git(runtime_root, "show", f"HEAD:{relative}"))
        pinned = selector(pinned_text)
        match = current == pinned
        all_match = all_match and match
        paths[label] = {
            "selected_sha256": _canonical_sha256(current),
            "selected_value_matches_pinned_head": match,
            "worktree_path_dirty": relative in dirty,
        }
    return {
        "pinned_head": str(_run_git(runtime_root, "rev-parse", "HEAD")).strip(),
        "selected_sources": paths,
        "selected_values_reconstructable_from_pinned_head": all_match,
    }


def _config_contract(roots: dict[str, Path], runtime_root: Path) -> dict[str, Any]:
    oidc_base = _safe_oidc_base((roots["auth_oidc"] / "config.yml").read_text(encoding="utf-8"))
    oidc_runtime = _safe_oidc_runtime(
        (runtime_root / "pylon_auth/configs/auth_oidc.yml").read_text(encoding="utf-8")
    )
    saml_base = _safe_saml_base((roots["auth_saml"] / "config.yml").read_text(encoding="utf-8"))
    core_runtime = _safe_runtime_auth_core(
        (runtime_root / "pylon_auth/configs/auth_core.yml").read_text(encoding="utf-8")
    )
    bootstrap = _safe_bootstrap(
        (runtime_root / "pylon_auth/configs/bootstrap.yml").read_text(encoding="utf-8")
    )
    oidc_admin = json.loads((roots["auth_oidc"] / "admin_schema.json").read_text(encoding="utf-8"))
    core_admin = json.loads((roots["auth_core"] / "admin_schema.json").read_text(encoding="utf-8"))

    oidc_base_fields = oidc_base["fields"]
    oidc_runtime_fields = oidc_runtime["fields"]
    missing_states = {"empty", "absent"}
    oidc_credentials_present = all(
        oidc_runtime_fields.get(field, oidc_base_fields.get(field)) not in missing_states
        for field in ("client_id", "client_secret")
    )
    oidc_metadata_present = (
        oidc_runtime_fields.get("metadata_endpoint") not in missing_states
    )
    oidc_static_endpoints_present = all(
        oidc_base_fields.get(field) not in missing_states
        for field in ("authorization_endpoint", "token_endpoint")
    )
    oidc_material_present = oidc_credentials_present and (
        oidc_metadata_present or oidc_static_endpoints_present
    )
    saml_material_present = all(
        saml_base["fields"][field] not in {"empty", "absent"}
        for field in (
            "sp_key",
            "sp_cert",
            "idp_cert",
            "authn_destination",
            "logout_destination",
            "saml_issuer",
        )
    )

    return {
        "checked_in_activation": {
            "auth_core_selected_provider": core_runtime["auth_provider"],
            "bootstrap_loads_oidc": "auth_oidc" in bootstrap["preordered_plugins"],
            "bootstrap_loads_saml": "auth_saml" in bootstrap["preordered_plugins"],
            "checked_in_oidc_required_material_present": oidc_material_present,
            "checked_in_saml_required_material_present": saml_material_present,
            "live_runtime_or_deployment_examined": False,
            "oidc_selected_by_checked_in_auth_core": core_runtime["auth_provider"] == "oidc",
            "ready_or_deployed_parity_claim": False,
            "saml_selected_by_checked_in_auth_core": core_runtime["auth_provider"] == "saml",
            "scope": "checked-in source and configuration only",
        },
        "merge_precedence": [
            "plugin config.yml base",
            "context.settings.configs.<plugin> seed",
            "configured /data/configs/<plugin>.yml provider override",
        ],
        "merge_source": "pylon/core/tools/module/descriptor.py#ModuleDescriptor.load_config",
        "oidc": {
            "admin_schema": {
                "present": True,
                "property_keys": sorted(oidc_admin.get("properties", {})),
                "secret_field_format": oidc_admin["properties"]["oidc_client_secret"].get("format"),
            },
            "base": oidc_base,
            "checked_in_override": oidc_runtime,
            "hidden_code_defaults": {
                "expiration_override": None,
                "login_mode": "post",
                "logout_mode": "get",
                "metadata_endpoint_verify": True,
                "require_email_verified": False,
                "target_response_type": "code",
                "target_scope": "openid profile email",
                "token_endpoint_auth": "basic",
                "token_endpoint_verify": True,
                "url_mode": "default",
                "without_expiration_claim_seconds": 86400,
                "without_jwt_public_key": "signature verification disabled",
            },
            "runtime_discovery_mutates_endpoints": [
                "authorization_endpoint",
                "token_endpoint",
                "userinfo_endpoint",
                "end_session_endpoint",
            ],
        },
        "saml": {
            "admin_schema": {
                "auth_core_provider_enum_contains_saml": "saml"
                in core_admin["properties"]["auth_provider"].get("enum", []),
                "present": (roots["auth_saml"] / "admin_schema.json").exists(),
            },
            "base": saml_base,
            "checked_in_override": {
                "path": "centry/pylon_auth/configs/auth_saml.yml",
                "present": (runtime_root / "pylon_auth/configs/auth_saml.yml").exists(),
            },
            "hidden_code_defaults": {
                "attributes_map": {},
                "authn_acs_url": "request-derived external ACS route",
                "authn_acs_url_add": True,
                "authn_nameid_format": "urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified",
                "authn_sign": True,
                "authn_verify": True,
                "expiration_override": None,
                "fallback_to_nameid_for_email": True,
                "logout_mode": "post",
                "logout_sign": True,
                "logout_verify": True,
                "response_remap": {},
                "without_session_expiration_seconds": 86400,
            },
            "idp_metadata_loading": "commented source only; not an active behavior",
        },
        "runtime_config_provider_limit": (
            "an external /data/configs provider or runtime bootstrap update can override "
            "the checked-in merge; neither source is executed by this exporter"
        ),
    }


def _behavior_contracts() -> list[dict[str, Any]]:
    return [
        {
            "id": "oidc.login",
            "current": {
                "correlation": (
                    "generate UUID, sign {uuid} as HS512 state, store target token under "
                    "flask.session.auth_oidc[uuid]"
                ),
                "pending_state_growth": (
                    "one session-map member is added per initiation and remains until that "
                    "specific callback consumes it; abandoned initiations have no explicit bound or TTL"
                ),
                "request": "target_to query defaults to empty string",
                "response": (
                    "redirect for login_mode=get; otherwise 200 auto-submit HTML POST with "
                    "response_type, client_id, redirect_uri, scope, and state"
                ),
            },
        },
        {
            "id": "oidc.callback",
            "current": {
                "claim_mapping": {
                    "attributes": "entire decoded ID-token claim object",
                    "expiration": "override, exp timestamp, or callback time plus 24 hours",
                    "provider": "oidc",
                    "provider_reference": "preferred_username, otherwise sub",
                    "sessionindex": "raw ID token",
                },
                "code_flow": (
                    "POST token request with basic or data client authentication; parse JSON "
                    "without explicit timeout, response-size bound, or status check"
                ),
                "direct_id_token_flow": "accept id_token from callback form/query",
                "state": (
                    "require signed state and matching session member; pop member before token "
                    "exchange, validation, provisioning, and session regeneration"
                ),
                "validation": (
                    "RS256 plus audience only when jwt_public_key is configured; otherwise "
                    "decode with signature verification disabled; require sub and optionally email_verified"
                ),
                "wire_inputs": "GET query or POST form",
            },
        },
        {
            "id": "oidc.logout",
            "current": {
                "local_mode": "destroy/regenerate local session immediately",
                "remote_modes": (
                    "GET redirect or 200 auto-submit POST with raw ID token as id_token_hint; "
                    "local session remains authenticated until callback"
                ),
                "state": "new signed UUID member stores target token in the current session",
            },
        },
        {
            "id": "oidc.logout_callback",
            "current": (
                "require signed state and current-session membership, pop it, then destroy and "
                "regenerate local session and redirect through verified target token"
            ),
        },
        {
            "id": "saml.login",
            "current": {
                "binding": "200 auto-submit HTTP-POST form carrying SAMLRequest and RelayState",
                "request": (
                    "random AuthnRequest ID, issuer, NameID policy, optional ACS URL; sign by "
                    "default with configured SP key/certificate"
                ),
                "state": "RelayState is the Auth Core signed target token; request ID is not persisted",
            },
        },
        {
            "id": "saml.acs",
            "current": {
                "claim_mapping": {
                    "attributes": "first value becomes scalar and later values form a list after optional remap",
                    "email": "mapped email or bare NameID fallback",
                    "expiration": "override, SessionNotOnOrAfter, or callback time plus 24 hours",
                    "provider": "saml",
                    "provider_reference": "bare NameID",
                    "sessionindex": "AuthnStatement SessionIndex or empty string",
                },
                "positive_signature_behavior": (
                    "authn_verify defaults true, pins configured idp_cert, and parses "
                    "SignXML verify_result.signed_xml"
                ),
                "protocol_checks": (
                    "checks Status Success and first NameID only; no RelayState transaction, "
                    "InResponseTo, request replay, issuer, audience, Destination, Recipient, "
                    "Conditions, or SubjectConfirmation validation"
                ),
                "wire_inputs": "GET query or POST form; base64/XML and decoded attribute sizes are unbounded",
            },
        },
        {
            "id": "saml.logout",
            "current": {
                "local_mode": "destroy/regenerate local session immediately",
                "remote_mode": (
                    "200 auto-submit HTTP-POST LogoutRequest with NameID and RelayState; signed "
                    "by default, but stored SessionIndex is omitted and local session remains valid"
                ),
            },
        },
        {
            "id": "saml.sls",
            "current": (
                "GET/POST base64 response; optional pinned-certificate signature verification, "
                "then unconditional local logout without Status, issuer, audience, destination, "
                "InResponseTo, RelayState, or request-correlation checks"
            ),
        },
        {
            "id": "shared.auth_context_commit",
            "current": {
                "flaw": (
                    "OIDC/SAML callbacks write done=true and provider state before Auth Init; "
                    "a processor failure returns denial without clearing that state, so a later "
                    "/auth request can accept the session"
                ),
                "success": (
                    "run registered processors, regenerate Flask session ID, write returned auth "
                    "context, verify target token or use default login URL, then redirect"
                ),
            },
        },
        {
            "id": "shared.identity_provisioning",
            "current": {
                "existing_mapping": "provider_ref globally resolves one user",
                "missing_mapping": (
                    "lower/derive email, link same-email user or create user, add root group 1"
                ),
                "per_login": (
                    "fire new_ai_user, touch last_login, fill missing name, and assign super_admin "
                    "only when exact provider_ref is configured and user has no global roles"
                ),
                "project_event": (
                    "eligible domains receive viewer and configured additional project roles for "
                    "existing global admin/super_admin identities"
                ),
            },
        },
        {
            "id": "shared.server_session",
            "current": {
                "browser_cookie": "signed session identifier with HttpOnly/path/domain/secure/lifetime settings",
                "record": "Redis-backed server session serialized with Python Pickle",
                "provider_state": (
                    "raw decoded OIDC claims/raw ID token or parsed SAML attributes/SessionIndex "
                    "remain in the authentication context"
                ),
            },
        },
        {
            "id": "shared.error_translation",
            "current": (
                "uncaught provider/parser exceptions reach Auth Core's broad handler; NotFound is "
                "404, while other exceptions return the access-denied response with status overridden to 400"
            ),
        },
    ]


def _consumer_contract() -> dict[str, Any]:
    return {
        "auth_core_dispatch": {
            "login": "/auth/login selects configured auth_provider and redirects to its registered login route",
            "logout": "/auth/logout selects session provider, falling back to configured auth_provider",
        },
        "gateway": {
            "main_exposure_prefix": "/auth",
            "main_rpc_consumer": (
                "pylon_main auth_mode=rpc calls auth_authorize and consumes X-Auth-Type, "
                "X-Auth-ID, and X-Auth-Reference"
            ),
        },
        "mapper_consumers": {
            "header": "configured projections read provider_attr including nameid after reopening referenced session",
            "json": "info projection includes raw context plus configured provider_attr paths",
            "rpc": "success mapper forwards type, ID, and raw session reference",
        },
        "profile_consumers": [
            "pylon_main auth current-user endpoint reads provider_attr.attributes.picture",
            "social auth_visitor event reads provider_attr.attributes.picture",
            "social current-author fallback reads provider_attr.attributes.picture",
        ],
        "ui": {
            "direct_logout": "/auth/logout from sidebar and settings",
            "popup": (
                "a redirected API response containing /auth/ and /login opens a UI "
                "reauthentication popup; UI state correlates popup completion, not provider assertions"
            ),
        },
    }


def _security_dispositions() -> list[dict[str, str]]:
    return [
        {
            "id": "oidc.external_contract",
            "migration": "preserve",
            "baseline": "provider route names, verified identity fields, provisioning, and redirect outcome",
            "requirement": "preserve wire/business parity unless an item below explicitly corrects it",
        },
        {
            "id": "oidc.oauth_transaction",
            "migration": "correct",
            "baseline": "signed UUID state and session membership, but no nonce or PKCE",
            "requirement": "one-time CSPRNG transaction bound to session/provider, nonce, PKCE, and same-origin bounded return target",
        },
        {
            "id": "oidc.token_validation",
            "migration": "correct",
            "baseline": "optional static RS256 key; signature verification is disabled when absent",
            "requirement": "verified discovery/JWKS with pinned issuer, audience, allowed algorithm, exp/nbf/iat policy, nonce, and required claims; no unverified mode",
        },
        {
            "id": "oidc.callback_and_outbound_bounds",
            "migration": "correct",
            "baseline": "duplicate/unbounded query or form and token/discovery HTTP without explicit time/body bounds",
            "requirement": "bounded duplicate-reject callback, admission before allocation, HTTPS-only bounded outbound client, status/content-type validation, and cancellation",
        },
        {
            "id": "oidc.secret_and_token_handling",
            "migration": "correct",
            "baseline": "callback/token/claims are debug logged and raw ID token is retained as sessionindex",
            "requirement": "no credential/token/assertion logs; retain only bounded verified claims needed by consumers and an explicitly reviewed logout hint, never access/refresh tokens",
        },
        {
            "id": "oidc.federated_logout",
            "migration": "phase",
            "baseline": "remote logout leaves local session valid until callback and sends raw ID token hint",
            "requirement": "invalidate locally first; add bounded provider logout as optional follow-up after login parity",
        },
        {
            "id": "saml.external_contract",
            "migration": "preserve",
            "baseline": "provider route names, NameID/attribute mapping, provisioning, and redirect outcome",
            "requirement": "preserve wire/business parity unless an item below explicitly corrects it",
        },
        {
            "id": "saml.signature_boundary",
            "migration": "preserve_and_strengthen",
            "baseline": "authn_verify defaults true, pins idp_cert, and consumes SignXML signed_xml, but unsigned mode exists",
            "requirement": "require a unique signed response or assertion from pinned trust in production; remove unsigned mode and signature-wrapping ambiguity",
        },
        {
            "id": "saml.protocol_semantics",
            "migration": "correct",
            "baseline": "Status and NameID only after signature parsing",
            "requirement": "enforce request ID/InResponseTo, issuer, audience, canonical Destination and Recipient, Conditions and SubjectConfirmation times with skew, and unique NameID/assertion selection",
        },
        {
            "id": "saml.replay_and_relay",
            "migration": "correct",
            "baseline": "AuthnRequest ID is not stored and RelayState is only a reusable signed target token",
            "requirement": "one-time session/provider-bound RelayState transaction, persisted AuthnRequest correlation, and replay consumption before provisioning",
        },
        {
            "id": "saml.input_and_logging",
            "migration": "correct",
            "baseline": "GET/POST, base64/XML/attributes are unbounded and response material is debug logged",
            "requirement": "bounded POST form by default, strict base64/XML parser and attribute limits, no assertion/claim logs; retain GET only for evidenced compatibility",
        },
        {
            "id": "saml.federated_logout",
            "migration": "correct",
            "baseline": "remote logout omits SessionIndex, retains local session, and SLS lacks protocol/correlation checks",
            "requirement": "invalidate locally first and defer SLO until request/response, RelayState, signature, Status, issuer, destination, and replay contracts are implemented",
        },
        {
            "id": "shared.auth_commit",
            "migration": "correct",
            "baseline": "done=true is persisted before provisioning and survives processor failure",
            "requirement": "verify, consume transaction, provision atomically, then rotate into authenticated session; failures leave no authenticated partial state",
        },
        {
            "id": "shared.identity_provisioning",
            "migration": "preserve_and_transactionalize",
            "baseline": "email/link/create/root-group/event/last-login/name/initial-admin effects can partially commit under current AUTOCOMMIT behavior",
            "requirement": "preserve decisions and visible effects in one typed transaction with explicit post-commit event delivery",
        },
        {
            "id": "shared.provider_reference",
            "migration": "preserve_then_migrate",
            "baseline": "OIDC preferred_username/sub and SAML NameID share one globally unique raw provider_ref namespace",
            "requirement": "preserve raw references for the first compatible slice; namespacing requires an explicit data migration and collision plan",
        },
        {
            "id": "shared.config_secret_distribution",
            "migration": "externalize",
            "baseline": "bootstrap announcements include parsed config and raw config_data; runtime.plugins APIs can read/export it, including SP keys and client secrets",
            "requirement": "store secret references only, resolve at the trusted provider adapter, redact runtime/admin surfaces, and never broadcast raw secret-bearing configuration",
        },
    ]


def _route_contracts(roots: dict[str, Path]) -> dict[str, dict[str, Any]]:
    records: dict[str, dict[str, Any]] = {}
    for provider, targets in ROUTE_TARGETS.items():
        prefix = f"/auth/{provider}"
        for relative, class_name, method_name, contract_id in targets:
            tree = _parse(roots[provider] / relative)
            node = _method(tree, class_name, method_name)
            route, declared = _route_spec(node)
            key = f"{provider}/{relative}#{class_name}.{method_name}"
            records[key] = {
                "behavior_contract_id": contract_id,
                "declared_methods": declared,
                "declared_route": route,
                "effective_methods": _effective_methods(declared),
                "effective_route": prefix + route,
                "provider": provider.removeprefix("auth_"),
            }
    return dict(sorted(records.items()))


def _method_route_matrix(route_contracts: dict[str, dict[str, Any]]) -> dict[str, dict[str, str]]:
    matrix: dict[str, dict[str, str]] = {}
    for record in route_contracts.values():
        direct = set(record["declared_methods"])
        methods: dict[str, str] = {}
        for method in record["effective_methods"]:
            if method == "HEAD":
                methods[method] = "framework.head_via_get"
            elif method == "OPTIONS":
                methods[method] = "framework.automatic_options"
            elif method in direct:
                methods[method] = record["behavior_contract_id"]
        matrix[record["effective_route"]] = methods
    return dict(sorted(matrix.items()))


def _source_hashes(roots: dict[str, Path]) -> dict[str, str]:
    return {
        f"{root_name}/{relative}": _file_sha256(roots[root_name] / relative)
        for root_name, relatives in ROOT_SOURCE_FILES.items()
        for relative in relatives
    }


def _selected_config_hashes(roots: dict[str, Path], runtime_root: Path) -> dict[str, str]:
    result: dict[str, str] = {}
    for root_name, relatives in ROOT_CONFIG_FILES.items():
        for relative in relatives:
            label = f"{root_name}/{relative}"
            selected = _safe_config_selector(label)(
                (roots[root_name] / relative).read_text(encoding="utf-8")
            )
            result[label] = _canonical_sha256(selected)
    for relative in RUNTIME_CONFIG_FILES:
        label = "centry/" + relative
        selected = _safe_config_selector(label)(
            (runtime_root / relative).read_text(encoding="utf-8")
        )
        result[label] = _canonical_sha256(selected)
    return dict(sorted(result.items()))


def _behavior_fingerprints(roots: dict[str, Path]) -> dict[str, dict[str, Any]]:
    result: dict[str, dict[str, Any]] = {}
    for root_name, targets in FINGERPRINT_TARGETS.items():
        for relative, class_name, method_name in targets:
            source = f"{root_name}/{relative}"
            tree = _parse(roots[root_name] / relative)
            node = _method(tree, class_name, method_name)
            result[f"{source}#{class_name}.{method_name}"] = _fingerprint(
                node, source, class_name, method_name
            )
    return dict(sorted(result.items()))


def _provenance(roots: dict[str, Path], runtime_root: Path) -> dict[str, Any]:
    logical_roots = {
        "auth_oidc": "centry/pylon_auth/plugins/auth_oidc",
        "auth_saml": "centry/pylon_auth/plugins/auth_saml",
        "auth_core": "centry/pylon_auth/plugins/auth_core",
        "auth_init": "centry/pylon_auth/plugins/auth_init",
        "auth_mappers": "centry/pylon_auth/plugins/auth_mappers",
        "main_auth": "centry/pylon_main/plugins/auth",
        "social": "centry/pylon_main/plugins/social",
        "elitea_core": "centry/pylon_main/plugins/elitea_core",
        "bootstrap": "centry/pylon_auth/plugins/bootstrap",
        "admin": "centry/pylon_main/plugins/admin",
        "pylon": "pylon",
        "ui": "EliteaUI",
    }
    result: dict[str, Any] = {}
    for root_name in ROOT_SOURCE_FILES:
        contract_paths = set(ROOT_SOURCE_FILES[root_name]) | set(
            ROOT_CONFIG_FILES.get(root_name, ())
        )
        result[f"{root_name}_repo"] = {
            **git_provenance(roots[root_name], contract_paths),
            "source_root": logical_roots[root_name],
        }
    result["runtime_config_repo"] = {
        **_selected_runtime_provenance(runtime_root),
        "source_root": "centry",
    }
    return result


def build_catalog(
    auth_oidc_root: Path,
    auth_saml_root: Path,
    auth_core_root: Path,
    auth_init_root: Path,
    auth_mappers_root: Path,
    main_auth_root: Path,
    social_root: Path,
    elitea_core_root: Path,
    bootstrap_root: Path,
    admin_root: Path,
    pylon_root: Path,
    ui_root: Path,
    runtime_root: Path,
) -> dict[str, Any]:
    roots = {
        "auth_oidc": auth_oidc_root.resolve(),
        "auth_saml": auth_saml_root.resolve(),
        "auth_core": auth_core_root.resolve(),
        "auth_init": auth_init_root.resolve(),
        "auth_mappers": auth_mappers_root.resolve(),
        "main_auth": main_auth_root.resolve(),
        "social": social_root.resolve(),
        "elitea_core": elitea_core_root.resolve(),
        "bootstrap": bootstrap_root.resolve(),
        "admin": admin_root.resolve(),
        "pylon": pylon_root.resolve(),
        "ui": ui_root.resolve(),
    }
    runtime_root = runtime_root.resolve()
    route_contracts = _route_contracts(roots)
    full_sources = sorted(
        f"{root_name}/{relative}"
        for root_name, relatives in ROOT_SOURCE_FILES.items()
        for relative in relatives
    )
    selected_configs = sorted(
        [
            f"{root_name}/{relative}"
            for root_name, relatives in ROOT_CONFIG_FILES.items()
            for relative in relatives
        ]
        + ["centry/" + relative for relative in RUNTIME_CONFIG_FILES]
    )
    return {
        "behavior_contracts": _behavior_contracts(),
        "behavior_fingerprints": _behavior_fingerprints(roots),
        "configuration_contract": _config_contract(roots, runtime_root),
        "consumer_contract": _consumer_contract(),
        "inference_limits": [
            "checked-in empty/inactive provider configuration is not proof of a live configured or deployed provider",
            "external runtime config-provider payloads, IdP metadata, certificates, and database plugin_config rows are not executed",
            "route source presence and bootstrap membership do not prove gateway reachability or successful provider handshakes",
            "provider-reference namespacing is intentionally deferred because current populated-database collisions were not migrated in this slice",
            "UI popup state correlates the popup result only and is not evidence of OIDC/SAML assertion validation",
            "the fixture is not full pylon_auth, pylon_main Auth, Admin, Social, or RBAC parity evidence",
        ],
        "method_route_matrix": _method_route_matrix(route_contracts),
        "provenance": _provenance(roots, runtime_root),
        "route_contracts": route_contracts,
        "schema_version": SCHEMA_VERSION,
        "scope": {
            "claim": "bounded current-baseline OIDC and SAML authentication evidence",
            "covered_providers": ["oidc", "saml"],
            "full_auth_parity_claim": False,
            "live_configured_or_deployed_parity_claim": False,
        },
        "security_dispositions": _security_dispositions(),
        "source_files_sha256": _source_hashes(roots),
        "source_inventory": {
            "full_byte_sources": full_sources,
            "secret_bearing_config_paths": sorted(SECRET_BEARING_CONFIG_PATHS),
            "selected_config_sources": selected_configs,
        },
        "source_reconstruction": {
            "full_byte_sources": (
                "hashes cover reviewed worktree bytes and provenance requires those exact "
                "contract paths to be reconstructable from each pinned repository head"
            ),
            "secret_config_policy": (
                "credential, key, certificate, and endpoint values are neither emitted nor "
                "raw-hashed; selected hashes cover only redacted presence/configuration shape"
            ),
            "selected_runtime_configuration": (
                "safe semantic selectors are compared with the Centry pinned head so unrelated "
                "runtime tuning does not invalidate this contract"
            ),
        },
        "selected_config_sha256": _selected_config_hashes(roots, runtime_root),
    }


def _serialized(catalog: dict[str, Any]) -> str:
    return json.dumps(catalog, indent=2, sort_keys=True) + "\n"


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--auth-oidc-root", type=Path, default=Path("../centry/pylon_auth/plugins/auth_oidc"))
    parser.add_argument("--auth-saml-root", type=Path, default=Path("../centry/pylon_auth/plugins/auth_saml"))
    parser.add_argument("--auth-core-root", type=Path, default=Path("../centry/pylon_auth/plugins/auth_core"))
    parser.add_argument("--auth-init-root", type=Path, default=Path("../centry/pylon_auth/plugins/auth_init"))
    parser.add_argument("--auth-mappers-root", type=Path, default=Path("../centry/pylon_auth/plugins/auth_mappers"))
    parser.add_argument("--main-auth-root", type=Path, default=Path("../centry/pylon_main/plugins/auth"))
    parser.add_argument("--social-root", type=Path, default=Path("../centry/pylon_main/plugins/social"))
    parser.add_argument("--elitea-core-root", type=Path, default=Path("../centry/pylon_main/plugins/elitea_core"))
    parser.add_argument("--bootstrap-root", type=Path, default=Path("../centry/pylon_auth/plugins/bootstrap"))
    parser.add_argument("--admin-root", type=Path, default=Path("../centry/pylon_main/plugins/admin"))
    parser.add_argument("--pylon-root", type=Path, default=Path("../pylon"))
    parser.add_argument("--ui-root", type=Path, default=Path("../EliteaUI"))
    parser.add_argument("--runtime-root", type=Path, default=Path("../centry"))
    parser.add_argument("--output", type=Path, default=Path("testdata/baseline/federated-auth-contracts.json"))
    parser.add_argument("--check", action="store_true")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    try:
        catalog = build_catalog(
            args.auth_oidc_root,
            args.auth_saml_root,
            args.auth_core_root,
            args.auth_init_root,
            args.auth_mappers_root,
            args.main_auth_root,
            args.social_root,
            args.elitea_core_root,
            args.bootstrap_root,
            args.admin_root,
            args.pylon_root,
            args.ui_root,
            args.runtime_root,
        )
        rendered = _serialized(catalog)
    except (OSError, ValueError, json.JSONDecodeError) as exc:
        print(json.dumps({"error": str(exc)}, sort_keys=True), file=sys.stderr)
        return 2

    if args.check:
        try:
            current = args.output.read_text(encoding="utf-8")
        except OSError as exc:
            print(json.dumps({"error": str(exc)}, sort_keys=True), file=sys.stderr)
            return 2
        if current != rendered:
            print(
                json.dumps(
                    {
                        "error": "reviewed federated-auth evidence changed",
                        "output": args.output.as_posix(),
                    },
                    sort_keys=True,
                ),
                file=sys.stderr,
            )
            return 1
        print(json.dumps({"ok": True, "output": args.output.as_posix()}, sort_keys=True))
        return 0

    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(rendered, encoding="utf-8")
    print(json.dumps({"ok": True, "output": args.output.as_posix()}, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
