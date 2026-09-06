"""
Local Repository Manager

Handles cloning and managing temporary local repositories for wiki generation.
Supports multi-provider Git operations (GitHub, GitLab, Bitbucket, Azure DevOps).
"""

import os
import shutil
import tempfile
import subprocess
import logging
from typing import Dict, Optional, Any, List
from pathlib import Path
from urllib.parse import urlparse

# Handle both relative and absolute imports
try:
    from .repo_providers import RepoProviderFactory, GitCloneConfig, ProviderType
except ImportError:
    from repo_providers import RepoProviderFactory, GitCloneConfig, ProviderType

logger = logging.getLogger(__name__)


class LocalRepositoryManager:
    """
    Manages local repository clones for filesystem-based processing.
    
    Supports multiple Git providers via toolkit configuration:
    - GitHub (github.com + Enterprise)
    - GitLab (gitlab.com + self-hosted)
    - Bitbucket (Cloud + Server/Data Center)
    - Azure DevOps (dev.azure.com + visualstudio.com)
    """
    
    def __init__(self, 
                 cache_dir: Optional[str] = None,
                 cleanup_on_exit: bool = True,
                 provider_type: Optional[str] = None,
                 toolkit_config: Optional[Dict[str, Any]] = None,
                 # New: Pre-built clone configuration (preferred)
                 clone_config: Optional[GitCloneConfig] = None,
                 # Legacy GitHub parameters (deprecated, use clone_config instead)
                 github_access_token: Optional[str] = None,
                 github_username: Optional[str] = None):
        """
        Initialize local repository manager.
        
        Args:
            cache_dir: Directory for caching cloned repositories
            cleanup_on_exit: Whether to cleanup temporary directories on exit
            provider_type: Provider type (github, gitlab, bitbucket, ado)
            toolkit_config: Toolkit configuration dict from platform
            clone_config: Pre-built GitCloneConfig (preferred method)
            github_access_token: [DEPRECATED] Use clone_config instead
            github_username: [DEPRECATED] Use clone_config instead
        """
        self.cache_dir = cache_dir or os.path.join(tempfile.gettempdir(), "wiki_builder_repos")
        self.cleanup_on_exit = cleanup_on_exit
        
        # Store pre-built clone config if provided (preferred)
        self._clone_config = clone_config
        
        # Provider configuration
        if clone_config:
            # Extract provider type and config from clone_config
            self.provider_type = clone_config.provider.value
            self.toolkit_config = {}  # Not needed when clone_config is provided
        else:
            self.provider_type = provider_type or "github"
            self.toolkit_config = toolkit_config or {}
        
        # Legacy GitHub support for backward compatibility
        if not clone_config and github_access_token and not toolkit_config:
            self.toolkit_config = {
                "base_url": "https://api.github.com",
                "access_token": github_access_token,
                "username": github_username,
            }
            self.provider_type = "github"
        
        # Also check environment variables for backward compatibility
        if not clone_config and not self.toolkit_config and os.getenv("GITHUB_ACCESS_TOKEN"):
            self.toolkit_config = {
                "base_url": "https://api.github.com",
                "access_token": os.getenv("GITHUB_ACCESS_TOKEN"),
                "username": os.getenv("GITHUB_USERNAME"),
            }
            self.provider_type = "github"
        
        self.active_repos: Dict[str, str] = {}  # repo_identifier -> local_path
        
        # Ensure cache directory exists
        os.makedirs(self.cache_dir, exist_ok=True)
        
    def get_repository_local_path(self, 
                                 repository_url: str, 
                                 branch: str = "main",
                                 force_reclone: bool = False,
                                 project: Optional[str] = None) -> str:
        """
        Get local path for repository, cloning if necessary.
        
        Clone paths include commit hash for isolation:
        - Different commits = different paths = no conflicts
        - Same commit = reuse existing clone = efficient
        
        Args:
            repository_url: Repository URL or owner/repo format
            branch: Branch to clone
            force_reclone: Whether to force reclone even if exists
            project: Project name (required for ADO, optional for others)
            
        Returns:
            Local path to repository root
        """
        # An ARTIFACT FOLDER is a source too, and there is nothing to clone:
        # elitea_deepwiki.artifact_source downloads the folder into the same
        # cache directory a clone would have produced, and everything after
        # this branch is unchanged. See that module for the identity, which
        # is a sha256 over the listing where a clone has a commit sha.
        from elitea_deepwiki.artifact_source import (
            is_artifact_source,
            materialise_artifact_source,
        )

        if is_artifact_source(repository_url):
            local_path = materialise_artifact_source(
                repository_url,
                branch,
                self.cache_dir,
                force=force_reclone,
            )
            self.active_repos[f"{repository_url}:{branch}"] = local_path
            return local_path

        # Build clone configuration using provider factory
        clone_config = self._get_clone_config(repository_url, branch, project)
        
        # Normalize repository identifier
        repo_name = clone_config.repo_identifier
        
        # Get remote HEAD commit hash to determine clone path
        # This ensures each commit version has its own isolated clone
        remote_commit = self._get_remote_head_commit(clone_config)
        commit_short = remote_commit[:8] if remote_commit else "latest"
        
        # Create path with commit hash for isolation: owner_repo_branch_abc12345/
        safe_name = repo_name.replace("/", "_")
        local_path = os.path.join(self.cache_dir, f"{safe_name}_{branch}_{commit_short}")
        
        # Build identifier with commit for caching
        repo_identifier = f"{repo_name}:{branch}:{commit_short}"
        
        # Check if already cloned and cached
        if not force_reclone and repo_identifier in self.active_repos:
            cached_path = self.active_repos[repo_identifier]
            if os.path.exists(cached_path):
                logger.info(f"Using cached repository: {cached_path}")
                return cached_path
        
        # Check if path already exists (from previous run)
        if not force_reclone and os.path.exists(local_path):
            logger.info(f"Using existing clone at: {local_path}")
            self.active_repos[repo_identifier] = local_path
            return local_path
        
        # Remove existing if force reclone
        if force_reclone and os.path.exists(local_path):
            logger.info(f"Removing existing repository for reclone: {local_path}")
            shutil.rmtree(local_path)
        
        # Clone fresh
        self._clone_repository(clone_config, local_path)
        
        # Cache the path
        self.active_repos[repo_identifier] = local_path
        
        return local_path
    
    def _get_clone_config(self, repository_url: str, branch: str, project: Optional[str] = None) -> GitCloneConfig:
        """
        Build GitCloneConfig from repository URL and toolkit configuration.
        
        Uses pre-built clone_config if available, otherwise builds from toolkit config.
        """
        # Use pre-built clone config if available and matches the requested repository
        if self._clone_config:
            # Check if the pre-built config matches the requested repository
            if (self._clone_config.repo_identifier == repository_url or 
                repository_url in self._clone_config.repo_identifier or
                self._clone_config.repo_identifier.endswith(repository_url.split('/')[-1])):
                # Return a copy with potentially overridden branch
                if branch != self._clone_config.branch:
                    # Need to rebuild with different branch
                    return RepoProviderFactory.from_toolkit_config(
                        provider_type=self.provider_type,
                        config=self.toolkit_config or {},
                        repository=repository_url,
                        branch=branch,
                        project=project,
                    )
                return self._clone_config
        
        # Build new config from toolkit configuration
        return RepoProviderFactory.from_toolkit_config(
            provider_type=self.provider_type,
            config=self.toolkit_config,
            repository=repository_url,
            branch=branch,
            project=project,
        )
    
    def _get_remote_head_commit(self, clone_config: GitCloneConfig) -> Optional[str]:
        """
        Get the HEAD commit hash from remote repository without cloning.
        
        This allows us to determine the clone path before cloning,
        ensuring commit-based isolation.
        """
        try:
            # Use git ls-remote to get remote HEAD commit
            cmd = ['git', 'ls-remote', clone_config.clone_url, f'refs/heads/{clone_config.branch}']
            
            result = subprocess.run(
                cmd,
                capture_output=True,
                text=True,
                check=False,
                timeout=30
            )
            
            if result.returncode == 0 and result.stdout.strip():
                # Output format: "<commit_hash>\trefs/heads/<branch>"
                commit_hash = result.stdout.strip().split()[0]
                logger.debug(f"Remote HEAD for {clone_config.repo_identifier}:{clone_config.branch} is {commit_hash[:8]}")
                return commit_hash
            else:
                logger.warning(f"Could not get remote HEAD: {result.stderr}")
                return None
                
        except subprocess.TimeoutExpired:
            logger.warning(f"Timeout getting remote HEAD for {clone_config.repo_identifier}")
            return None
        except Exception as e:
            logger.warning(f"Failed to get remote HEAD: {e}")
            return None
    
    def _normalize_repository_name(self, repository_url: str) -> str:
        """Convert various repository URL formats to owner/repo format"""
        if repository_url.startswith(('http://', 'https://')):
            # Parse URL to extract owner/repo
            parsed = urlparse(repository_url)
            path = parsed.path.strip('/')
            if path.endswith('.git'):
                path = path[:-4]
            return path
        elif repository_url.startswith('git@'):
            # SSH format: git@github.com:owner/repo.git
            import re
            match = re.match(r'^git@[^:]+:([^/]+/[^/]+?)(?:\.git)?$', repository_url)
            if not match:
                raise ValueError("Invalid SSH repository format")
            return match.group(1)
        else:
            # Assume it's already in owner/repo format - strip .git suffix if present
            if repository_url.endswith('.git'):
                return repository_url[:-4]
            return repository_url
    
    def _clone_repository(self, clone_config: GitCloneConfig, local_path: str) -> None:
        """Clone repository to local path using provider-aware configuration."""
        logger.info(f"Cloning repository {clone_config.repo_identifier} (branch: {clone_config.branch}) to {local_path}")
        
        try:
            # Ensure parent directory exists
            os.makedirs(os.path.dirname(local_path), exist_ok=True)
            
            # Clone with depth=1 for faster cloning (latest commit only)
            cmd = [
                'git', 'clone', 
                '--branch', clone_config.branch,
                '--depth', '1',
                '--single-branch',
                clone_config.clone_url,
                local_path
            ]
            
            # Log command without exposing token
            logger.debug(f"Executing: git clone --branch {clone_config.branch} --depth 1 --single-branch {clone_config.safe_url} {local_path}")
            
            result = subprocess.run(
                cmd, 
                capture_output=True, 
                text=True, 
                check=False
            )
            
            if result.returncode != 0:
                # Sanitize error message to remove credentials
                error_msg = clone_config.sanitize_output(result.stderr)
                logger.error(f"Git clone failed: {error_msg}")
                
                # Provide helpful error messages based on error type
                # NOTE: Order matters! More specific checks MUST come before generic ones.
                
                # 1. Authentication errors
                if "Authentication failed" in error_msg or "Permission denied" in error_msg:
                    raise RuntimeError(
                        f"Authentication failed for repository '{clone_config.repo_identifier}'. "
                        "Please check your access token and permissions."
                    )
                # 2. Branch not found errors (check BEFORE generic "not found")
                elif "Could not find remote branch" in error_msg:
                    raise RuntimeError(
                        f"Branch '{clone_config.branch}' does not exist in repository '{clone_config.repo_identifier}'. "
                        "Please check the branch name and try again."
                    )
                elif "Remote branch" in error_msg and "not found" in error_msg:
                    raise RuntimeError(
                        f"Branch '{clone_config.branch}' not found in repository '{clone_config.repo_identifier}'. "
                        "Please verify the branch name exists."
                    )
                # 3. Repository not found (generic "not found" check LAST)
                elif "Repository not found" in error_msg:
                    raise RuntimeError(
                        f"Repository not found: '{clone_config.repo_identifier}'. "
                        "Please verify the repository name and your access permissions."
                    )
                # 4. Fallback for unknown errors
                else:
                    raise RuntimeError(f"Git clone failed for '{clone_config.repo_identifier}': {error_msg}")
            
            logger.info(f"Successfully cloned repository to {local_path}")
            
        except Exception as e:
            logger.error(f"Failed to clone repository {clone_config.repo_identifier}: {e}")
            # Cleanup partial clone
            if os.path.exists(local_path):
                shutil.rmtree(local_path)
            raise
    
    def get_repository_info(self, local_path: str) -> Dict[str, Any]:
        """Get repository information from local clone"""
        # A materialised artifact folder is not a git tree. Its identity was
        # recorded beside it when it was downloaded; answering from that
        # record is what keeps the commit-shaped cache key content-derived.
        from elitea_deepwiki.artifact_source import artifact_repository_info

        artifact_info = artifact_repository_info(local_path)
        if artifact_info is not None:
            return artifact_info

        try:
            # Get current commit hash
            commit_result = subprocess.run(
                ['git', 'rev-parse', 'HEAD'],
                cwd=local_path,
                capture_output=True,
                text=True,
                check=True
            )
            commit_hash = commit_result.stdout.strip()
            
            # Get current branch
            branch_result = subprocess.run(
                ['git', 'branch', '--show-current'],
                cwd=local_path,
                capture_output=True,
                text=True,
                check=True
            )
            current_branch = branch_result.stdout.strip()
            
            # Get remote URL
            remote_result = subprocess.run(
                ['git', 'remote', 'get-url', 'origin'],
                cwd=local_path,
                capture_output=True,
                text=True,
                check=True
            )
            remote_url = remote_result.stdout.strip()
            
            return {
                'local_path': local_path,
                'commit_hash': commit_hash,
                'branch': current_branch,
                'remote_url': remote_url,
                'exists': True
            }
            
        except subprocess.CalledProcessError as e:
            logger.warning(f"Failed to get repository info: {e}")
            return {
                'local_path': local_path,
                'exists': os.path.exists(local_path),
                'error': str(e)
            }
    
    def list_files(self, local_path: str, extensions: Optional[List[str]] = None) -> List[str]:
        """List all files in repository with optional extension filtering"""
        if not os.path.exists(local_path):
            return []
        
        files = []
        for root, dirs, filenames in os.walk(local_path):
            # Skip .git directory
            if '.git' in dirs:
                dirs.remove('.git')
            
            for filename in filenames:
                if extensions:
                    if any(filename.endswith(ext) for ext in extensions):
                        rel_path = os.path.relpath(os.path.join(root, filename), local_path)
                        files.append(rel_path)
                else:
                    rel_path = os.path.relpath(os.path.join(root, filename), local_path)
                    files.append(rel_path)
        
        return files
    
    def cleanup_repository(self, repository_url: str, branch: str = "main") -> None:
        """Remove cached repository clone"""
        repo_name = self._normalize_repository_name(repository_url)
        repo_identifier = f"{repo_name}:{branch}"
        
        if repo_identifier in self.active_repos:
            local_path = self.active_repos[repo_identifier]
            if os.path.exists(local_path):
                logger.info(f"Cleaning up repository: {local_path}")
                shutil.rmtree(local_path)
            del self.active_repos[repo_identifier]
    
    def cleanup_all(self) -> None:
        """Remove all cached repository clones"""
        for repo_identifier, local_path in list(self.active_repos.items()):
            if os.path.exists(local_path):
                logger.info(f"Cleaning up repository: {local_path}")
                shutil.rmtree(local_path)
        
        self.active_repos.clear()
        
        # Also clean up cache directory if empty
        if os.path.exists(self.cache_dir) and not os.listdir(self.cache_dir):
            os.rmdir(self.cache_dir)
    
    def __enter__(self):
        """Context manager entry"""
        return self
    
    def __exit__(self, exc_type, exc_val, exc_tb):
        """Context manager exit with cleanup"""
        if self.cleanup_on_exit:
            self.cleanup_all()


# Convenience function for quick repository access
def clone_and_get_path(
    repository_url: str, 
    branch: str = "main",
    cache_dir: Optional[str] = None,
    provider_type: str = "github",
    toolkit_config: Optional[Dict[str, Any]] = None,
    project: Optional[str] = None,
    # Legacy parameters for backward compatibility
    github_access_token: Optional[str] = None,
    github_username: Optional[str] = None,
) -> str:
    """
    Quick function to clone repository and get local path.
    
    Args:
        repository_url: Repository URL or owner/repo format
        branch: Branch to clone
        cache_dir: Optional cache directory
        provider_type: Provider type (github, gitlab, bitbucket, ado)
        toolkit_config: Toolkit configuration dict from platform
        project: Project name (required for ADO, optional for others)
        github_access_token: [DEPRECATED] Use toolkit_config instead
        github_username: [DEPRECATED] Use toolkit_config instead
        
    Returns:
        Local path to repository root
    """
    manager = LocalRepositoryManager(
        cache_dir=cache_dir, 
        cleanup_on_exit=False,
        provider_type=provider_type,
        toolkit_config=toolkit_config,
        github_access_token=github_access_token,
        github_username=github_username,
    )
    return manager.get_repository_local_path(repository_url, branch, project=project)
