import importlib.util
import unittest
from pathlib import Path


PATCH_PATH = Path(__file__).resolve().parents[1] / "fly" / "patch_device_stream.py"


def load_patch_module():
    spec = importlib.util.spec_from_file_location("patch_device_stream_under_test", PATCH_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class PatchDeviceStreamTest(unittest.TestCase):
    def test_mcp_vision_frame_forwards_to_bridge(self):
        patch_device_stream = load_patch_module()
        source = '''    elif "method" in payload:
        method = payload["method"]
        logger.bind(tag=TAG).info(f"收到MCP客户端请求: {method}")

'''

        patched = patch_device_stream.patch_mcp_handler_source(source)

        self.assertIn('"xiaoli/vision_frame"', patched)
        self.assertIn('"xiaoli_bridge"', patched)
        self.assertIn("stream_bridge.publish_stream_frame_base64", patched)
        self.assertNotIn("xiaoli_admin", patched)


if __name__ == "__main__":
    unittest.main()
