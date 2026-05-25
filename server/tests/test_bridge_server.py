import asyncio
import importlib.util
import json
import os
import sys
import types
import unittest
from pathlib import Path
from unittest.mock import patch


BRIDGE_PATH = Path(__file__).resolve().parents[1] / "fly" / "xiaoli_bridge.py"


class DummyLogger:
    def bind(self, **kwargs):
        return self

    def info(self, *args, **kwargs):
        pass

    def error(self, *args, **kwargs):
        pass


def load_bridge_module():
    config_mod = types.ModuleType("config")
    logger_mod = types.ModuleType("config.logger")
    logger_mod.setup_logging = lambda: DummyLogger()
    config_mod.logger = logger_mod

    mcp_handler_mod = types.ModuleType("core.providers.tools.device_mcp.mcp_handler")

    async def call_mcp_tool(handler, mcp_client, tool_name, arguments, timeout=30):
        return json.dumps(
            {
                "device": handler.device_id,
                "tool": tool_name,
                "arguments": arguments,
                "timeout": timeout,
            },
            ensure_ascii=False,
        )

    mcp_handler_mod.call_mcp_tool = call_mcp_tool

    send_audio_mod = types.ModuleType("core.handle.sendAudioHandle")

    async def send_tts_message(handler, event):
        handler.sent_events.append(event)

    send_audio_mod.send_tts_message = send_tts_message

    dto_mod = types.ModuleType("core.providers.tts.dto.dto")

    class SentenceType:
        FIRST = "first"
        MIDDLE = "middle"
        LAST = "last"

    class ContentType:
        ACTION = "action"
        TEXT = "text"

    class TTSMessageDTO:
        def __init__(self, **kwargs):
            self.__dict__.update(kwargs)

    dto_mod.SentenceType = SentenceType
    dto_mod.ContentType = ContentType
    dto_mod.TTSMessageDTO = TTSMessageDTO

    aiohttp_mod = types.ModuleType("aiohttp")
    web_mod = types.ModuleType("aiohttp.web")

    class HTTPException(Exception):
        def __init__(self, text="", status=None):
            super().__init__(text)
            self.text = text
            self.status = status

    class HTTPBadRequest(HTTPException):
        pass

    class HTTPNotFound(HTTPException):
        pass

    class HTTPConflict(HTTPException):
        pass

    class Application:
        def add_routes(self, routes):
            self.routes = routes

    class AppRunner:
        def __init__(self, app):
            self.app = app

        async def setup(self):
            pass

    class TCPSite:
        def __init__(self, runner, host, port):
            self.runner = runner
            self.host = host
            self.port = port

        async def start(self):
            pass

    class Response:
        def __init__(self, text="", status=200, content_type=None):
            self.text = text
            self.status = status
            self.content_type = content_type

    def json_response(payload, status=200, dumps=json.dumps):
        return Response(text=dumps(payload), status=status, content_type="application/json")

    web_mod.Application = Application
    web_mod.AppRunner = AppRunner
    web_mod.TCPSite = TCPSite
    web_mod.Response = Response
    web_mod.json_response = json_response
    web_mod.get = lambda path, handler: ("GET", path, handler)
    web_mod.post = lambda path, handler: ("POST", path, handler)
    web_mod.HTTPBadRequest = HTTPBadRequest
    web_mod.HTTPNotFound = HTTPNotFound
    web_mod.HTTPConflict = HTTPConflict
    aiohttp_mod.web = web_mod

    modules = {
        "config": config_mod,
        "config.logger": logger_mod,
        "core.providers.tools.device_mcp.mcp_handler": mcp_handler_mod,
        "core.handle.sendAudioHandle": send_audio_mod,
        "core.providers.tts.dto.dto": dto_mod,
        "aiohttp": aiohttp_mod,
        "aiohttp.web": web_mod,
    }
    with patch.dict(sys.modules, modules):
        spec = importlib.util.spec_from_file_location("xiaoli_bridge_under_test", BRIDGE_PATH)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module


class BridgeServerTest(unittest.TestCase):
    def setUp(self):
        self.bridge = load_bridge_module()

    def make_server(self, ws_server):
        with patch.dict(os.environ, {"XIAOLI_BRIDGE_ENABLED": "true"}, clear=True):
            return self.bridge.XiaoliBridgeServer({"tool_call_timeout": 30}, ws_server)

    def test_devices_are_read_from_websocket_registry(self):
        class FakeWebSocketServer:
            async def list_admin_devices(self):
                return [{"device_id": "device-1", "mcp_ready": True, "tool_count": 2}]

        server = self.make_server(FakeWebSocketServer())
        response = asyncio.run(server.handle_devices(object()))

        payload = json.loads(response.text)
        self.assertEqual(payload["devices"][0]["device_id"], "device-1")

    def test_call_uses_mcp_tool_on_active_handler(self):
        class FakeRequest:
            async def json(self):
                return {
                    "device_id": "device-1",
                    "tool": "self.get_device_status",
                    "arguments": {"verbose": True},
                    "timeout": 9,
                }

        class FakeMcpClient:
            tools = []

            async def is_ready(self):
                return True

        class FakeHandler:
            device_id = "device-1"
            mcp_client = FakeMcpClient()

        class FakeWebSocketServer:
            async def get_admin_connection(self, device_id):
                return FakeHandler()

        server = self.make_server(FakeWebSocketServer())
        response = asyncio.run(server.handle_call(FakeRequest()))

        payload = json.loads(response.text)
        self.assertTrue(payload["ok"])
        self.assertEqual(payload["result"]["tool"], "self.get_device_status")
        self.assertEqual(payload["result"]["arguments"], {"verbose": True})

    def test_speak_queues_text_to_device_tts(self):
        class FakeRequest:
            async def json(self):
                return {"device_id": "device-1", "text": "请坐直。"}

        class FakeQueue:
            def __init__(self):
                self.items = []

            def put(self, item):
                self.items.append(item)

            def qsize(self):
                return len(self.items)

        class FakeTTS:
            def __init__(self):
                self.tts_text_queue = FakeQueue()
                self.tts_audio_queue = FakeQueue()
                self.stored = []

            def store_tts_text(self, sentence_id, text):
                self.stored.append((sentence_id, text))

        class FakeHandler:
            device_id = "device-1"
            websocket = object()
            client_is_speaking = False

            def __init__(self):
                self.tts = FakeTTS()
                self.sent_events = []

        handler = FakeHandler()

        class FakeWebSocketServer:
            async def get_admin_connection(self, device_id):
                return handler

        server = self.make_server(FakeWebSocketServer())
        response = asyncio.run(server.handle_speak(FakeRequest()))

        payload = json.loads(response.text)
        self.assertTrue(payload["ok"])
        self.assertEqual(handler.tts.stored[0][1], "请坐直。")
        self.assertEqual(handler.sent_events, ["start"])

    def test_speak_queues_full_text_as_one_tts_item_even_when_tts_one_sentence_exists(self):
        class FakeRequest:
            async def json(self):
                return {"device_id": "device-1", "text": "请坐直，认真学习。"}

        class FakeQueue:
            def __init__(self):
                self.items = []

            def put(self, item):
                self.items.append(item)

            def qsize(self):
                return len(self.items)

        class FakeTTS:
            def __init__(self):
                self.tts_text_queue = FakeQueue()
                self.tts_audio_queue = FakeQueue()
                self.one_sentence_calls = []

            def store_tts_text(self, sentence_id, text):
                self.stored_text = text

            def tts_one_sentence(self, handler, content_type, content_detail=None, sentence_id=None):
                self.one_sentence_calls.append(content_detail)

        class FakeHandler:
            device_id = "device-1"
            websocket = object()
            client_is_speaking = False

            def __init__(self):
                self.tts = FakeTTS()
                self.sent_events = []

        handler = FakeHandler()

        class FakeWebSocketServer:
            async def get_admin_connection(self, device_id):
                return handler

        server = self.make_server(FakeWebSocketServer())
        response = asyncio.run(server.handle_speak(FakeRequest()))

        payload = json.loads(response.text)
        self.assertTrue(payload["ok"])
        self.assertEqual(handler.tts.one_sentence_calls, [])
        text_items = [
            item
            for item in handler.tts.tts_text_queue.items
            if getattr(item, "content_type", None) == self.bridge.ContentType.TEXT
        ]
        self.assertEqual(len(text_items), 1)
        self.assertEqual(text_items[0].content_detail, "请坐直，认真学习。")


if __name__ == "__main__":
    unittest.main()
