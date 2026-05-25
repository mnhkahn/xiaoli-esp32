import importlib.util
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
PATCH_PATH = ROOT / "fly" / "patch_langsmith.py"
HELPER_PATH = ROOT / "fly" / "xiaoli_langsmith.py"


def load_module(path: Path, name: str):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class LangSmithPatchTest(unittest.TestCase):
    def test_helper_redacts_vision_image_inputs(self):
        helper = load_module(HELPER_PATH, "xiaoli_langsmith_under_test")
        payload = {
            "messages": [
                {
                    "role": "user",
                    "content": [
                        {"type": "text", "text": "看一下这张图"},
                        {
                            "type": "image_url",
                            "image_url": {
                                "url": "data:image/jpeg;base64,abc123",
                            },
                        },
                    ],
                }
            ]
        }

        redacted = helper.redact_langsmith_inputs(payload)

        self.assertEqual(redacted["messages"][0]["content"][0]["text"], "看一下这张图")
        self.assertEqual(redacted["messages"][0]["content"][1]["image_url"]["url"], "[image omitted]")

    def test_patch_wraps_llm_and_vllm_openai_clients(self):
        patch_langsmith = load_module(PATCH_PATH, "patch_langsmith_under_test")
        llm_source = (
            "import httpx\n"
            "import openai\n"
            "class LLMProvider:\n"
            "    def __init__(self):\n"
            "        self.api_key = 'k'\n"
            "        self.base_url = 'https://api.siliconflow.cn/v1/'\n"
            "        self.model_name = 'Qwen/Qwen3-8B'\n"
            "        custom_timeout = 30\n"
            "        self.client = openai.OpenAI(api_key=self.api_key, base_url=self.base_url, timeout=custom_timeout)\n"
        )
        vllm_source = (
            "import openai\n"
            "class VLLMProvider:\n"
            "    def __init__(self):\n"
            "        self.api_key = 'k'\n"
            "        self.base_url = 'https://api.siliconflow.cn/v1/'\n"
            "        self.model_name = 'Qwen/Qwen3-VL-8B-Instruct'\n"
            "        self.client = openai.OpenAI(api_key=self.api_key, base_url=self.base_url)\n"
        )

        patched_llm = patch_langsmith.patch_llm_source(llm_source)
        patched_vllm = patch_langsmith.patch_vllm_source(vllm_source)

        self.assertIn("from xiaoli_langsmith import wrap_openai_client", patched_llm)
        self.assertIn('provider="LLM"', patched_llm)
        self.assertIn("from xiaoli_langsmith import wrap_openai_client", patched_vllm)
        self.assertIn('provider="VLLM"', patched_vllm)


if __name__ == "__main__":
    unittest.main()
