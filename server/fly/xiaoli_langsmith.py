import os
from urllib.parse import urlparse


TRUE_VALUES = {"1", "true", "yes", "on"}
IMAGE_PLACEHOLDER = "[image omitted]"


def _env_enabled(name: str, default: str = "false") -> bool:
    return os.environ.get(name, default).strip().lower() in TRUE_VALUES


def _has_langsmith_key() -> bool:
    return bool(os.environ.get("LANGSMITH_API_KEY") or os.environ.get("LANGCHAIN_API_KEY"))


def _safe_host(url: str | None) -> str:
    if not url:
        return ""
    try:
        return urlparse(url).netloc or url
    except Exception:
        return ""


def redact_langsmith_inputs(value):
    """Remove inline image payloads before LangSmith receives trace inputs."""
    if isinstance(value, dict):
        return {key: redact_langsmith_inputs(child) for key, child in value.items()}
    if isinstance(value, list):
        return [redact_langsmith_inputs(item) for item in value]
    if isinstance(value, str) and value.startswith("data:image/"):
        return IMAGE_PLACEHOLDER
    return value


def wrap_openai_client(client, *, provider: str, model_name: str | None = None, base_url: str | None = None):
    if not _env_enabled("LANGSMITH_TRACING") or not _has_langsmith_key():
        return client

    try:
        from langsmith import Client
        from langsmith.wrappers import wrap_openai
    except Exception:
        return client

    metadata = {
        "xiaoli_provider": provider,
        "model_name": model_name or "",
        "base_url_host": _safe_host(base_url),
    }
    tags = ["xiaoli-server", f"provider:{provider.lower()}"]

    try:
        langsmith_client = Client(hide_inputs=redact_langsmith_inputs)
        return wrap_openai(
            client,
            tracing_extra={"client": langsmith_client, "metadata": metadata, "tags": tags},
            chat_name=f"Xiaoli{provider}Chat",
            completions_name=f"Xiaoli{provider}Completion",
        )
    except Exception:
        return client
