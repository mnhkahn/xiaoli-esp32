from pathlib import Path


PROJECT_DIR = Path("/opt/xiaozhi-esp32-server")
LLM_PATH = PROJECT_DIR / "core" / "providers" / "llm" / "openai" / "openai.py"
VLLM_PATH = PROJECT_DIR / "core" / "providers" / "vllm" / "openai.py"
HELPER_IMPORT = "from xiaoli_langsmith import wrap_openai_client\n"


def _ensure_helper_import(source: str) -> str:
    if HELPER_IMPORT in source:
        return source
    if "import openai\n" not in source:
        raise RuntimeError("Cannot find OpenAI import to patch")
    return source.replace("import openai\n", "import openai\n" + HELPER_IMPORT, 1)


def patch_llm_source(source: str) -> str:
    source = _ensure_helper_import(source)
    old = "self.client = openai.OpenAI(api_key=self.api_key, base_url=self.base_url, timeout=custom_timeout)"
    new = (
        "self.client = wrap_openai_client(\n"
        "            openai.OpenAI(api_key=self.api_key, base_url=self.base_url, timeout=custom_timeout),\n"
        '            provider="LLM",\n'
        "            model_name=self.model_name,\n"
        "            base_url=self.base_url,\n"
        "        )"
    )
    if new in source:
        return source
    if old not in source:
        raise RuntimeError("Cannot find LLM OpenAI client initialization to patch")
    return source.replace(old, new, 1)


def patch_vllm_source(source: str) -> str:
    source = _ensure_helper_import(source)
    old = "self.client = openai.OpenAI(api_key=self.api_key, base_url=self.base_url)"
    new = (
        "self.client = wrap_openai_client(\n"
        "            openai.OpenAI(api_key=self.api_key, base_url=self.base_url),\n"
        '            provider="VLLM",\n'
        "            model_name=self.model_name,\n"
        "            base_url=self.base_url,\n"
        "        )"
    )
    if new in source:
        return source
    if old not in source:
        raise RuntimeError("Cannot find VLLM OpenAI client initialization to patch")
    return source.replace(old, new, 1)


def _patch_file(path: Path, patcher):
    source = path.read_text(encoding="utf-8")
    patched = patcher(source)
    path.write_text(patched, encoding="utf-8")


def main():
    _patch_file(LLM_PATH, patch_llm_source)
    _patch_file(VLLM_PATH, patch_vllm_source)


if __name__ == "__main__":
    main()
