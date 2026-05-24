import importlib.util
import asyncio
import os
import sys
import types
import unittest
from pathlib import Path
from unittest.mock import patch


ADMIN_PATH = Path(__file__).resolve().parents[1] / "fly" / "xiaoli_admin.py"


class DummyLogger:
    def bind(self, **kwargs):
        return self

    def info(self, *args, **kwargs):
        pass


def load_admin_module():
    config = types.ModuleType("config")
    config_logger = types.ModuleType("config.logger")
    config_logger.setup_logging = lambda: DummyLogger()

    core = types.ModuleType("core")
    providers = types.ModuleType("core.providers")
    tools = types.ModuleType("core.providers.tools")
    device_mcp = types.ModuleType("core.providers.tools.device_mcp")
    mcp_handler = types.ModuleType("core.providers.tools.device_mcp.mcp_handler")

    async def call_mcp_tool(*args, **kwargs):
        return "{}"

    mcp_handler.call_mcp_tool = call_mcp_tool

    modules = {
        "config": config,
        "config.logger": config_logger,
        "core": core,
        "core.providers": providers,
        "core.providers.tools": tools,
        "core.providers.tools.device_mcp": device_mcp,
        "core.providers.tools.device_mcp.mcp_handler": mcp_handler,
    }

    with patch.dict(sys.modules, modules):
        spec = importlib.util.spec_from_file_location("xiaoli_admin_under_test", ADMIN_PATH)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module


admin = load_admin_module()


class AdminTokenAuthTest(unittest.TestCase):
    def make_server(self, extra_env=None):
        env = {
            "XIAOLI_ADMIN_ENABLED": "true",
            "ADMIN_ACCESS_TOKEN": "test-token-0123456789abcdef",
            "ADMIN_SESSION_SECRET": "session-secret-0123456789abcdef0",
        }
        env.update(extra_env or {})
        with patch.dict(os.environ, env, clear=True):
            return admin.XiaoliAdminServer({}, object())

    def test_token_auth_does_not_require_logto_config(self):
        server = self.make_server()

        server._validate_config()

        self.assertEqual(server.auth_mode, "token")

    def test_valid_admin_token_creates_signed_session(self):
        server = self.make_server()
        server._validate_config()

        response = server._create_token_login_response(
            "test-token-0123456789abcdef",
            "/admin",
        )

        self.assertEqual(response.status, 302)
        self.assertEqual(response.headers["Location"], "/admin")
        cookie_value = response.cookies[admin.SESSION_COOKIE].value
        session = server._verify(cookie_value)
        self.assertEqual(session["user"]["sub"], "token-admin")
        self.assertEqual(session["user"]["name"], "Admin Token")

    def test_wrong_admin_token_is_rejected(self):
        server = self.make_server()
        server._validate_config()

        with self.assertRaises(admin.web.HTTPUnauthorized):
            server._create_token_login_response("wrong-token", "/admin")

    def test_dashboard_refreshes_tools_before_quick_actions(self):
        server = self.make_server()

        html = server._dashboard_html({"sub": "token-admin"})

        self.assertIn("async function ensureToolsReady", html)
        self.assertIn("await ensureToolsReady()", html)

    def test_dashboard_renders_photo_preview_blocks(self):
        server = self.make_server()

        html = server._dashboard_html({"sub": "token-admin"})

        self.assertIn("function renderResult", html)
        self.assertIn("photoPreview", html)
        self.assertIn("preview.images", html)

    def test_build_result_preview_extracts_images_and_text(self):
        server = self.make_server()
        payload = {
            "image_url": "https://example.com/camera.jpg",
            "analysis": "桌面上有一本书。",
            "nested": {
                "base64": "aGVsbG8=",
                "message": "光线正常。",
            },
        }

        preview = server._build_result_preview(payload)

        self.assertEqual(preview["images"][0], "https://example.com/camera.jpg")
        self.assertEqual(preview["images"][1], "data:image/jpeg;base64,aGVsbG8=")
        self.assertIn("桌面上有一本书。", preview["text"])
        self.assertIn("光线正常。", preview["text"])


class DelayedMcpClient:
    def __init__(self):
        self.calls = 0

    async def is_ready(self):
        self.calls += 1
        await asyncio.sleep(0)
        return self.calls >= 3


class AdminMcpReadinessTest(unittest.IsolatedAsyncioTestCase):
    def make_server(self):
        env = {
            "XIAOLI_ADMIN_ENABLED": "true",
            "ADMIN_ACCESS_TOKEN": "test-token-0123456789abcdef",
            "ADMIN_SESSION_SECRET": "session-secret-0123456789abcdef0",
        }
        with patch.dict(os.environ, env, clear=True):
            return admin.XiaoliAdminServer({}, object())

    async def test_waits_briefly_for_mcp_tools_to_be_ready(self):
        server = self.make_server()
        client = DelayedMcpClient()

        ready = await server._wait_for_mcp_ready(client, timeout=0.2, interval=0)

        self.assertTrue(ready)
        self.assertEqual(client.calls, 3)


if __name__ == "__main__":
    unittest.main()
