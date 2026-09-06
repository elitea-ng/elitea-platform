"""Repository configuration extraction — copied from the legacy handler.

``_extract_repo_config_from_toolkit`` and its four helpers are lifted out of
``deepwiki_plugin/methods/invoke.py`` at revision
``ce679f11dc31c209cc67f13565b286d5bb28ce58``. They are adapter code, not engine
code — they normalise whatever shape the platform's toolkit expansion produced
into the ``repo_config`` dict the engine's tools expect — so they live here
rather than in the copied ``engine/`` package.

They are copied rather than rewritten for the same reason the engine is: 140
lines across four providers (GitHub, GitLab, Bitbucket, Azure DevOps), each
with its own precedence chain over a dozen differently-prefixed keys, several
of which exist only because the UI and the API disagreed about where a value
lives. Retyping that is how a provider quietly stops resolving its branch.
Only the GitHub path is covered by a P0 fixture
(``generation/composed_result.json``'s ``engine_call.repo_config``); the other
three are carried on trust, which is an argument for copying, not against.

TWO MODIFICATIONS, named here because the copy is otherwise verbatim.

1. The legacy code logged its suspicious-ADO warning through Pylon's ``log``.
   That is the stdlib logger below.
2. A FIFTH provider type, ``artifact``, which the legacy plugin never had: a
   wiki whose source is a folder in the invoking project's artifact store
   rather than a git remote. It is a new branch, placed BEFORE the legacy
   four-provider test and returning early, so no legacy path changes shape.
   See ``elitea_deepwiki.artifact_source`` for what the engine then does with
   it, and ``run/repoconfig.go`` for the Go twin of this branch.

Nothing else differs.
"""

from __future__ import annotations

import logging
from typing import Any, Dict, Optional

from .artifact_source import (
    ARTIFACT_SCHEME,
    is_artifact_source,
    parse_artifact_source,
    repo_config_for,
    source_from_configuration,
)

log = logging.getLogger(__name__)


_TOOLKIT_PROVIDER_KEYS = (
    'github_configuration',
    'gitlab_configuration',
    'bitbucket_configuration',
    'ado_configuration',
    # The fifth, added here rather than only in the branch below so that the
    # prefixed and unprefixed spellings merge the same way every other
    # provider's do. The UI and the API disagree about the prefix for all of
    # them, and an artifact source is no exception.
    'artifact_configuration',
)


ARTIFACT_CONFIGURATION_KEYS = (
    'artifact_configuration',
    'toolkit_configuration_artifact_configuration',
)


def _extract_artifact_source(repo_settings: Dict[str, Any]) -> Optional[Dict[str, Any]]:
    """The fifth provider: a folder in the project's artifact store.

    Returns the normalised ``repo_config``, or ``None`` when this payload
    names no artifact source — in which case the legacy four-provider chain
    below runs exactly as it always did.

    Two spellings are accepted, because two layers name the source. The
    toolkit carries an ``artifact_configuration`` block with a bucket and an
    optional folder prefix; a request that was already normalised carries the
    ``artifact://`` repository string alone. Both resolve to the same source.
    """
    if not isinstance(repo_settings, dict):
        return None

    branch = (
        repo_settings.get('active_branch')
        or repo_settings.get('toolkit_configuration_active_branch')
        or repo_settings.get('version_label')
        or repo_settings.get('branch')
        or 'main'
    )

    for key in ARTIFACT_CONFIGURATION_KEYS:
        source = source_from_configuration(repo_settings.get(key))
        if source is not None:
            return repo_config_for(source, branch)

    repository = repo_settings.get('repository')
    if is_artifact_source(repository):
        return repo_config_for(parse_artifact_source(repository), branch)
    return None


def _payload_contains_provider_key(params: Dict[str, Any], provider_key: str) -> bool:
    """Check known toolkit payload locations for a provider-specific config key."""
    sources = [
        params,
        params.get('code_toolkit'),
        params.get('toolkit_configuration_code_toolkit'),
        params.get('toolkit_configuration_code_repository'),
        params.get('code_repository'),
    ]

    for source in sources:
        if not isinstance(source, dict):
            continue
        if provider_key in source:
            return True
        for nested in (source.get('settings'), source.get('toolkit_config'), _extract_configuration_parameters(source)):
            if isinstance(nested, dict) and provider_key in nested:
                return True

    return False


def _merge_dicts(*values: Any) -> Dict[str, Any]:
    """Shallow-merge dict values, ignoring non-dicts."""
    merged: Dict[str, Any] = {}
    for value in values:
        if isinstance(value, dict):
            merged.update(value)
    return merged


def _extract_configuration_parameters(source: Dict[str, Any]) -> Dict[str, Any]:
    """Extract toolkit configuration payload from known wrapper shapes."""
    configuration = source.get('configuration')
    if isinstance(configuration, dict) and isinstance(configuration.get('parameters'), dict):
        return configuration.get('parameters') or {}
    return {}


def _merge_provider_configs(settings: Dict[str, Any]) -> Dict[str, Any]:
    """Merge prefixed/unprefixed provider configs that may be split by UI/API shape."""
    merged = dict(settings)
    for provider_key in _TOOLKIT_PROVIDER_KEYS:
        prefixed_key = f'toolkit_configuration_{provider_key}'
        provider_config = _merge_dicts(settings.get(provider_key), settings.get(prefixed_key))
        if provider_config:
            merged[provider_key] = provider_config
            merged[prefixed_key] = provider_config
    return merged


def _merge_toolkit_payload(source: Any) -> Dict[str, Any]:
    """Merge toolkit_config/configuration/settings into one repo-config payload."""
    if not isinstance(source, dict):
        return {}

    wrapper_fields = {key: value for key, value in source.items() if key not in ('settings', 'toolkit_config', 'configuration')}
    sources = (
        wrapper_fields,
        source.get('toolkit_config'),
        _extract_configuration_parameters(source),
        source.get('settings'),
    )
    merged = _merge_dicts(*sources)

    for provider_key in _TOOLKIT_PROVIDER_KEYS:
        prefixed_key = f'toolkit_configuration_{provider_key}'
        provider_config = _merge_dicts(
            *(candidate.get(provider_key) for candidate in sources if isinstance(candidate, dict)),
            *(candidate.get(prefixed_key) for candidate in sources if isinstance(candidate, dict)),
        )
        if provider_config:
            merged[provider_key] = provider_config
            merged[prefixed_key] = provider_config

    return merged



def _extract_repo_config_from_toolkit(params: Dict[str, Any]) -> Dict[str, Any]:
    """
    Extract repository configuration from expanded code_toolkit.
    
    Supports: github, gitlab, bitbucket, ado_repos, and — added here — artifact
    
    Returns a normalized repo_config dict with:
        - provider_type: str (github, gitlab, bitbucket, ado_repos)
        - provider_config: dict (the provider-specific configuration)
        - repository: str (repository identifier)
        - branch: str (branch name)
        - project: str or None (for Bitbucket/ADO)
        - is_cloud: bool or None (for Bitbucket)
    """
    code_toolkit = (
        params.get('code_toolkit')
        or params.get('toolkit_configuration_code_toolkit')
        or params.get('toolkit_configuration_code_repository')
        or params.get('code_repository')
        or {}
    )
    
    # Initialize with defaults
    repo_config = {
        'provider_type': 'github',
        'provider_config': {},
        'repository': None,
        'branch': 'main',
        'project': None,
        'is_cloud': None,
    }
    
    repo_settings = {}
    if isinstance(code_toolkit, dict):
        repo_settings = _merge_toolkit_payload(code_toolkit)

    if not repo_settings and isinstance(params, dict):
        repo_settings = _merge_provider_configs(params)

    # ELITEA-PLATFORM ADDITION. The artifact source is tested FIRST and
    # returns early: it shares none of the four providers' key precedence, and
    # a payload that names one names nothing else.
    artifact_config = _extract_artifact_source(repo_settings)
    if artifact_config is None and isinstance(params, dict):
        artifact_config = _extract_artifact_source(params)
    if artifact_config is not None:
        return artifact_config

    if isinstance(repo_settings, dict) and any(
        key in repo_settings
        for key in (
            'github_configuration', 'gitlab_configuration', 'bitbucket_configuration', 'ado_configuration',
            'toolkit_configuration_github_configuration', 'toolkit_configuration_gitlab_configuration',
            'toolkit_configuration_bitbucket_configuration', 'toolkit_configuration_ado_configuration',
        )
    ):
        if 'github_configuration' in repo_settings or 'toolkit_configuration_github_configuration' in repo_settings:
            github_config = repo_settings.get('github_configuration') or repo_settings.get('toolkit_configuration_github_configuration') or {}
            repo_config['provider_type'] = 'github'
            repo_config['provider_config'] = github_config
            repo_config['repository'] = (
                repo_settings.get('repository')
                or repo_settings.get('github_repository')
                or repo_settings.get('toolkit_configuration_github_repository')
            )
            repo_config['branch'] = (
                repo_settings.get('active_branch')
                or repo_settings.get('toolkit_configuration_active_branch')
                or repo_settings.get('base_branch')
                or repo_settings.get('toolkit_configuration_base_branch')
                or repo_settings.get('branch', 'main')
            )
        elif 'gitlab_configuration' in repo_settings or 'toolkit_configuration_gitlab_configuration' in repo_settings:
            gitlab_config = repo_settings.get('gitlab_configuration') or repo_settings.get('toolkit_configuration_gitlab_configuration') or {}
            repo_config['provider_type'] = 'gitlab'
            repo_config['provider_config'] = gitlab_config
            repo_config['repository'] = repo_settings.get('repository') or repo_settings.get('toolkit_configuration_repository')
            repo_config['branch'] = (
                repo_settings.get('branch')
                or repo_settings.get('toolkit_configuration_branch')
                or repo_settings.get('active_branch')
                or repo_settings.get('toolkit_configuration_active_branch')
                or repo_settings.get('base_branch')
                or repo_settings.get('toolkit_configuration_base_branch', 'main')
            )
        elif 'bitbucket_configuration' in repo_settings or 'toolkit_configuration_bitbucket_configuration' in repo_settings:
            bitbucket_config = repo_settings.get('bitbucket_configuration') or repo_settings.get('toolkit_configuration_bitbucket_configuration') or {}
            repo_config['provider_type'] = 'bitbucket'
            repo_config['provider_config'] = bitbucket_config
            repo_config['repository'] = repo_settings.get('repository') or repo_settings.get('toolkit_configuration_repository')
            repo_config['branch'] = (
                repo_settings.get('branch')
                or repo_settings.get('toolkit_configuration_branch')
                or repo_settings.get('active_branch')
                or repo_settings.get('toolkit_configuration_active_branch')
                or repo_settings.get('base_branch')
                or repo_settings.get('toolkit_configuration_base_branch', 'main')
            )
            repo_config['project'] = repo_settings.get('project') or repo_settings.get('toolkit_configuration_project')
            repo_config['is_cloud'] = repo_settings.get('cloud') or repo_settings.get('toolkit_configuration_cloud')
        elif 'ado_configuration' in repo_settings or 'toolkit_configuration_ado_configuration' in repo_settings:
            ado_config = repo_settings.get('ado_configuration') or repo_settings.get('toolkit_configuration_ado_configuration') or {}
            repo_config['provider_type'] = 'ado_repos'
            repo_config['provider_config'] = ado_config
            repo_config['repository'] = (
                repo_settings.get('repository_id')
                or repo_settings.get('toolkit_configuration_repository_id')
                or repo_settings.get('repository')
                or repo_settings.get('toolkit_configuration_repository')
            )
            repo_config['branch'] = (
                repo_settings.get('active_branch')
                or repo_settings.get('toolkit_configuration_active_branch')
                or repo_settings.get('base_branch')
                or repo_settings.get('toolkit_configuration_base_branch')
                or repo_settings.get('branch', 'main')
            )
            repo_config['project'] = ado_config.get('project') or repo_settings.get('project') or repo_settings.get('toolkit_configuration_project')
        else:
            # Fallback: assume GitHub with legacy structure
            repo_config['provider_type'] = 'github'
            repo_config['provider_config'] = repo_settings.get('github_configuration', {})
            repo_config['repository'] = repo_settings.get('repository') or repo_settings.get('github_repository')
            repo_config['branch'] = repo_settings.get('base_branch') or repo_settings.get('active_branch', 'main')
    else:
        # Legacy fallback - direct parameters
        repo_config['provider_type'] = 'github'
        repo_config['provider_config'] = params.get('github_configuration', {})
        repo_config['repository'] = params.get('github_repository')
        repo_config['branch'] = params.get('github_base_branch') or params.get('github_branch', 'main')

    if _payload_contains_provider_key(params, 'ado_configuration') and (
        repo_config.get('provider_type') != 'ado_repos' or not repo_config.get('repository')
    ):
        log.warning(
            'Suspicious ADO repo config extraction: provider_type=%s repository=%s branch=%s project=%s',
            repo_config.get('provider_type'),
            repo_config.get('repository'),
            repo_config.get('branch'),
            repo_config.get('project'),
        )

    return repo_config


__all__ = ["ARTIFACT_SCHEME", "_extract_artifact_source", "_extract_repo_config_from_toolkit"]
