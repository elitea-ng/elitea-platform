"""Stable safe failures shared by offline and delivered execution."""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(slots=True)
class WorkerError(Exception):
    code: str
    safe_message: str
    exit_code: int = 6
    retryable: bool = False

    def __str__(self) -> str:
        return f"{self.code}: {self.safe_message}"


class InvalidInput(WorkerError):
    def __init__(self, safe_message: str = "The execution input is invalid.") -> None:
        super().__init__("INVALID_INPUT", safe_message, exit_code=2)


class ResourceExhausted(WorkerError):
    def __init__(self, safe_message: str = "The execution exceeded an approved resource limit.") -> None:
        super().__init__("RESOURCE_EXHAUSTED", safe_message, exit_code=2)


# The registered budget-refusal messages (#6732). Main maps each one, under
# MODEL_BUDGET_EXHAUSTED, to the public code the chat keys its copy on
# (PROJECT_BUDGET_EXHAUSTED / MEMBER_BUDGET_EXHAUSTED). The Rust worker and
# testdata/proto/runtime/v1/model_failure_policies.json state the same text.
PROJECT_BUDGET_EXHAUSTED_MESSAGE = (
    "The shared model budget of this project is exhausted. Requests are "
    "unavailable until the budget resets or an administrator raises the limit."
)
MEMBER_BUDGET_EXHAUSTED_MESSAGE = (
    "Your budget in this project is exhausted. Requests are unavailable until "
    "the budget resets or an administrator raises your limit."
)
BUDGET_EXHAUSTED_MESSAGES = frozenset(
    {PROJECT_BUDGET_EXHAUSTED_MESSAGE, MEMBER_BUDGET_EXHAUSTED_MESSAGE}
)


# The unscoped refusal: no Elitea ceiling is known to have refused the call,
# for example a provider's own quota or billing refusal. It is the codec's
# MODEL_BUDGET_EXHAUSTED default, and Main maps it to MODEL_BUDGET_EXHAUSTED.
UNSCOPED_BUDGET_EXHAUSTED_MESSAGE = (
    "The model budget is exhausted. Ask an administrator to check the project "
    "budget or provider billing before retrying."
)


class ModelBudgetExhausted(WorkerError):
    """A model budget refused the call. Carries only the refusing scope."""

    def __init__(self, scope: str = "unknown") -> None:
        if scope == "member":
            message = MEMBER_BUDGET_EXHAUSTED_MESSAGE
        elif scope == "project":
            message = PROJECT_BUDGET_EXHAUSTED_MESSAGE
        else:
            message = UNSCOPED_BUDGET_EXHAUSTED_MESSAGE
        super().__init__("MODEL_BUDGET_EXHAUSTED", message, exit_code=2)


class UnsupportedCapability(WorkerError):
    def __init__(self, safe_message: str = "The requested capability is not supported.") -> None:
        super().__init__("UNSUPPORTED_CAPABILITY", safe_message, exit_code=3)


class IncompatibleVersion(WorkerError):
    def __init__(self, safe_message: str = "The requested contract version is not compatible.") -> None:
        super().__init__("INCOMPATIBLE_VERSION", safe_message, exit_code=3)


class AuthorizationFailure(WorkerError):
    def __init__(self, safe_message: str = "Execution authorization failed.") -> None:
        super().__init__("AUTHORIZATION_FAILED", safe_message, exit_code=4)


class DependencyUnavailable(WorkerError):
    def __init__(self, safe_message: str = "A required runtime dependency is unavailable.") -> None:
        super().__init__("DEPENDENCY_UNAVAILABLE", safe_message, exit_code=5, retryable=True)


class ExecutionDraining(DependencyUnavailable):
    def __init__(self, safe_message: str = "The execution is draining.") -> None:
        super().__init__(safe_message)


class DeadlineExceeded(WorkerError):
    def __init__(self, safe_message: str = "The execution deadline was exceeded.") -> None:
        super().__init__("DEADLINE_EXCEEDED", safe_message, exit_code=5, retryable=True)


class ExecutionCancelled(WorkerError):
    def __init__(self, safe_message: str = "Execution was cancelled.") -> None:
        super().__init__("CANCELLED", safe_message)


class OutputCancellationWon(ExecutionCancelled):
    """Exact bound server result: cancellation beat this terminal output."""

    def __init__(self) -> None:
        super().__init__(
            "Execution cancellation won before this output became durable."
        )


class OutputDeadlineWon(DeadlineExceeded):
    """Exact bound server result: the deadline beat this terminal output."""

    def __init__(self) -> None:
        super().__init__(
            "The execution deadline won before this output became durable."
        )


class InternalFailure(WorkerError):
    def __init__(self) -> None:
        super().__init__("INTERNAL", "The runtime operation failed.")


class AmbiguousExecutionRecovery(WorkerError):
    """The prior SDK invocation may have started and cannot be repeated."""

    def __init__(self) -> None:
        # Runtime error messages are canonicalized by code before crossing the
        # worker boundary. Keep the in-process error aligned with that contract;
        # operators diagnose the ambiguous invocation from Main's durable
        # invocation_state, never from a worker-supplied free-form message.
        super().__init__("INTERNAL", "The runtime operation failed.")
