import importlib.util
import json
import tempfile
import unittest
from pathlib import Path


TOOL_PATH = Path(__file__).resolve().parents[1] / "tools" / "xiaoli_hwtest.py"


def load_tool():
    spec = importlib.util.spec_from_file_location("xiaoli_hwtest_under_test", TOOL_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class XiaoliHwtestToolTest(unittest.TestCase):
    def test_discovery_payload_advertises_local_ota_url(self):
        tool = load_tool()

        payload = json.loads(tool.discovery_payload("192.168.1.23", 8080, "dev-token").decode("utf-8"))

        self.assertEqual(payload["service"], "xiaoli-hwtest")
        self.assertEqual(payload["version"], 1)
        self.assertEqual(payload["ota_url"], "http://192.168.1.23:8080/xiaozhi/ota/")
        self.assertEqual(payload["token"], "dev-token")

    def test_docker_command_exposes_web_and_bridge_and_sets_local_urls(self):
        tool = load_tool()

        command = tool.build_docker_command(
            image="xiaoli-server:hwtest",
            host="192.168.1.23",
            public_port=8080,
            bridge_port=8005,
            admin_port=8004,
            device_ids="28:84:85:8c:ef:f4",
            auth_key="0123456789abcdef0123456789abcdef",
            internal_token="local-internal-token",
        )

        self.assertEqual(command[:4], ["docker", "run", "--rm", "--name"])
        self.assertIn("xiaoli-hwtest", command)
        self.assertIn("8080:8080", command)
        self.assertIn("8005:8005", command)
        self.assertIn("8004:8004", command)
        self.assertIn("PUBLIC_BASE_URL=http://192.168.1.23:8080", command)
        self.assertIn("PUBLIC_WS_URL=ws://192.168.1.23:8080/xiaozhi/v1/", command)
        self.assertIn("PUBLIC_VISION_URL=http://192.168.1.23:8080/mcp/vision/explain", command)
        self.assertIn("XIAOLI_BRIDGE_HOST=0.0.0.0", command)
        self.assertIn("ALLOWED_DEVICE_IDS=28:84:85:8c:ef:f4", command)
        self.assertIn("XIAOLI_ADMIN_INTERNAL_TOKEN=local-internal-token", command)

    def test_local_file_url_serves_parent_directory_and_quotes_filename(self):
        tool = load_tool()

        with tempfile.TemporaryDirectory() as tmp:
            audio = Path(tmp) / "test sound.ogg"
            audio.write_bytes(b"OggS")

            directory, url = tool.local_file_url(audio, "192.168.1.23", 9000)

        self.assertEqual(directory, audio.parent.resolve())
        self.assertEqual(url, "http://192.168.1.23:9000/test%20sound.ogg")


if __name__ == "__main__":
    unittest.main()
