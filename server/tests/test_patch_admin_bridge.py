import importlib.util
import unittest
from pathlib import Path


PATCH_PATH = Path(__file__).resolve().parents[1] / "fly" / "patch_admin.py"


def load_patch_module():
    spec = importlib.util.spec_from_file_location("patch_admin_under_test", PATCH_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class PatchAdminBridgeTest(unittest.TestCase):
    def test_patch_app_source_starts_bridge_not_python_admin(self):
        patch_admin = load_patch_module()
        source = (
            "from core.websocket_server import WebSocketServer\n"
            "async def main():\n"
            "    ota_server = SimpleHttpServer(config)\n"
            "    ota_task = asyncio.create_task(ota_server.start())\n"
            "    try:\n"
            "        await asyncio.wait(\n"
            "            [stdin_task, ws_task, ota_task] if ota_task else [stdin_task, ws_task],\n"
            "            timeout=3.0,\n"
            "            return_when=asyncio.ALL_COMPLETED,\n"
            "        )\n"
            "    finally:\n"
            "        if ota_task:\n"
            "            ota_task.cancel()\n"
        )

        patched = patch_admin.patch_app_source(source)

        self.assertIn("from xiaoli_bridge import XiaoliBridgeServer", patched)
        self.assertIn("bridge_server = XiaoliBridgeServer(config, ws_server)", patched)
        self.assertIn("bridge_task = asyncio.create_task(bridge_server.start())", patched)
        self.assertIn("tasks.append(bridge_task)", patched)
        self.assertIn("bridge_task.cancel()", patched)
        self.assertNotIn("xiaoli_admin", patched)

    def test_patch_websocket_source_tracks_admin_connections(self):
        patch_admin = load_patch_module()
        source = (
            "class WebSocketServer:\n"
            "    def __init__(self):\n"
            "        self.auth = AuthManager(secret_key=secret_key, expire_seconds=expire_seconds)\n"
            "    async def start(self):\n"
            "        pass\n"
            "    async def handler(self):\n"
            "        handler = ConnectionHandler(\n"
            "            self.config,\n"
            "            self._vad,\n"
            "            self._asr,\n"
            "            self._llm,\n"
            "            self._memory,\n"
            "            self._intent,\n"
            "            self,  # 传入server实例\n"
            "        )\n"
            "        try:\n"
            "            await handler.handle_connection(websocket)\n"
            "        except Exception as e:\n"
            "            self.logger.bind(tag=TAG).error(f\"处理连接时出错: {e}\")\n"
            "        finally:\n"
            "            # 强制关闭连接（如果还没有关闭的话）\n"
        )

        patched = patch_admin.patch_websocket_source(source)

        self.assertIn("self.active_connections = {}", patched)
        self.assertIn("async def register_connection", patched)
        self.assertIn("await self.register_connection(handler)", patched)
        self.assertIn("await self.unregister_connection(handler)", patched)


if __name__ == "__main__":
    unittest.main()
