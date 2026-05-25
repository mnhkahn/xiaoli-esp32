from __future__ import annotations

import asyncio
import json
import os
import time
import uuid
from typing import Any

from aiohttp import web
from config.logger import setup_logging
from core.handle.sendAudioHandle import send_tts_message
from core.providers.tools.device_mcp.mcp_handler import call_mcp_tool
from core.providers.tts.dto.dto import ContentType, SentenceType, TTSMessageDTO


TAG = __name__


def _env_bool(name: str, default: str = "false") -> bool:
    return os.environ.get(name, default).strip().lower() in {"1", "true", "yes", "on"}


def _json_response(payload: dict[str, Any], status: int = 200) -> web.Response:
    return web.json_response(payload, status=status, dumps=lambda obj: json.dumps(obj, ensure_ascii=False))


class XiaoliBridgeServer:
    def __init__(self, config: dict[str, Any], ws_server: Any):
        self.config = config
        self.ws_server = ws_server
        try:
            setattr(self.ws_server, "xiaoli_bridge", self)
        except Exception:
            pass
        self.logger = setup_logging()
        self.enabled = _env_bool("XIAOLI_BRIDGE_ENABLED", "true")
        self.host = os.environ.get("XIAOLI_BRIDGE_HOST", "127.0.0.1")
        self.port = int(os.environ.get("XIAOLI_BRIDGE_PORT", "8005"))
        self.go_admin_base_url = os.environ.get(
            "XIAOLI_GO_ADMIN_BASE_URL",
            f"http://127.0.0.1:{os.environ.get('XIAOLI_ADMIN_PORT', '8004')}",
        ).rstrip("/")
        self.internal_token = os.environ.get("XIAOLI_ADMIN_INTERNAL_TOKEN") or os.environ.get(
            "ADMIN_SESSION_SECRET", ""
        )
        self.mcp_ready_wait_seconds = float(os.environ.get("ADMIN_MCP_READY_WAIT_SECONDS", "5"))
        self.speak_wait_timeout = float(os.environ.get("ADMIN_SPEAK_WAIT_TIMEOUT_SECONDS", "300"))

    async def start(self):
        if not self.enabled:
            self.logger.bind(tag=TAG).info("Xiaoli bridge server is disabled")
            return

        app = web.Application()
        app.add_routes(
            [
                web.get("/bridge/devices", self.handle_devices),
                web.get("/bridge/tools", self.handle_tools),
                web.post("/bridge/call", self.handle_call),
                web.post("/bridge/speak", self.handle_speak),
            ]
        )

        runner = web.AppRunner(app)
        await runner.setup()
        site = web.TCPSite(runner, self.host, self.port)
        await site.start()
        self.logger.bind(tag=TAG).info(f"Xiaoli bridge server listening on {self.host}:{self.port}")
        while True:
            await asyncio.sleep(3600)

    async def handle_devices(self, request: web.Request) -> web.Response:
        devices = await self.ws_server.list_admin_devices()
        return _json_response({"devices": devices})

    async def handle_tools(self, request: web.Request) -> web.Response:
        handler = await self._get_handler_for_request(request)
        mcp_client = getattr(handler, "mcp_client", None)
        if not mcp_client or not await self._wait_for_mcp_ready(mcp_client):
            return _json_response({"tools": [], "ready": False})
        return _json_response({"tools": mcp_client.get_available_tools(), "ready": True})

    async def handle_call(self, request: web.Request) -> web.Response:
        try:
            body = await request.json()
        except Exception:
            raise web.HTTPBadRequest(text="invalid json")

        device_id = str(body.get("device_id") or "")
        tool_name = str(body.get("tool") or "")
        arguments = body.get("arguments") or {}
        timeout = int(body.get("timeout") or self.config.get("tool_call_timeout", 30) or 30)
        timeout = max(1, min(timeout, 120))
        if not device_id or not tool_name:
            raise web.HTTPBadRequest(text="device_id and tool are required")
        if not isinstance(arguments, dict):
            raise web.HTTPBadRequest(text="arguments must be an object")

        handler = await self.ws_server.get_admin_connection(device_id)
        if not handler:
            raise web.HTTPNotFound(text="device is not online")
        mcp_client = getattr(handler, "mcp_client", None)
        if not mcp_client or not await self._wait_for_mcp_ready(mcp_client):
            raise web.HTTPConflict(text="device MCP is not ready")

        started = time.time()
        try:
            resolved_tool_name = self._resolve_mcp_tool_name(mcp_client, tool_name)
            raw = await call_mcp_tool(handler, mcp_client, resolved_tool_name, arguments, timeout=timeout)
            elapsed_ms = int((time.time() - started) * 1000)
            return _json_response(
                {
                    "ok": True,
                    "result": self._try_json(raw),
                    "raw": raw,
                    "elapsed_ms": elapsed_ms,
                }
            )
        except Exception as exc:
            elapsed_ms = int((time.time() - started) * 1000)
            return _json_response({"ok": False, "error": str(exc), "elapsed_ms": elapsed_ms}, status=500)

    async def handle_speak(self, request: web.Request) -> web.Response:
        try:
            body = await request.json()
        except Exception:
            raise web.HTTPBadRequest(text="invalid json")
        device_id = str(body.get("device_id") or "").strip()
        text = str(body.get("text") or "").strip()
        if not device_id:
            raise web.HTTPBadRequest(text="device_id is required")
        if not text:
            raise web.HTTPBadRequest(text="text is required")

        handler = await self._get_speak_handler(device_id)
        sentence_id = await self._start_speak_text(handler, text)
        return _json_response(
            {
                "ok": True,
                "status": "queued",
                "device_id": device_id,
                "sentence_id": sentence_id,
                "tts_ready": True,
            }
        )

    def publish_stream_frame_base64(
        self,
        device_id: str,
        content_type: str,
        encoded_body: str,
        metadata: dict[str, Any] | None = None,
    ):
        if not self.internal_token:
            self.logger.bind(tag=TAG).error("Cannot forward stream frame: missing XIAOLI_ADMIN_INTERNAL_TOKEN")
            return
        asyncio.create_task(
            self._post_stream_frame_base64(device_id, content_type, encoded_body, metadata or {})
        )

    async def _post_stream_frame_base64(
        self,
        device_id: str,
        content_type: str,
        encoded_body: str,
        metadata: dict[str, Any],
    ):
        from aiohttp import ClientSession

        payload = {
            "device_id": device_id,
            "content_type": content_type,
            "data": encoded_body,
            "stream_id": metadata.get("stream_id", ""),
            "seq": metadata.get("seq", ""),
            "timestamp_ms": metadata.get("timestamp_ms", ""),
        }
        try:
            async with ClientSession() as session:
                async with session.post(
                    self.go_admin_base_url + "/admin/internal/stream/frame",
                    json=payload,
                    headers={"X-Xiaoli-Internal-Token": self.internal_token},
                    timeout=5,
                ) as response:
                    if response.status >= 400:
                        text = await response.text()
                        self.logger.bind(tag=TAG).error(
                            f"Forward stream frame failed: {response.status} {text[:120]}"
                        )
        except Exception as exc:
            self.logger.bind(tag=TAG).error(f"Forward stream frame failed: {exc}")

    async def _get_handler_for_request(self, request: web.Request):
        device_id = request.query.get("device_id", "")
        if device_id:
            handler = await self.ws_server.get_admin_connection(device_id)
            if not handler:
                raise web.HTTPNotFound(text="device is not online")
            return handler
        devices = await self.ws_server.list_admin_devices()
        if len(devices) != 1:
            raise web.HTTPBadRequest(text="device_id is required")
        handler = await self.ws_server.get_admin_connection(devices[0]["device_id"])
        if not handler:
            raise web.HTTPNotFound(text="device is not online")
        return handler

    async def _wait_for_mcp_ready(
        self,
        mcp_client: Any,
        timeout: float | None = None,
        interval: float = 0.2,
    ) -> bool:
        deadline = time.monotonic() + (self.mcp_ready_wait_seconds if timeout is None else timeout)
        while True:
            try:
                if await mcp_client.is_ready():
                    return True
            except Exception:
                return False
            if time.monotonic() >= deadline:
                return False
            await asyncio.sleep(interval)

    def _resolve_mcp_tool_name(self, mcp_client: Any, tool_name: str) -> str:
        safe_name = "".join(ch if ch.isalnum() or ch in "_-" else "_" for ch in tool_name)
        names = {tool_name, safe_name}
        try:
            tools = mcp_client.get_available_tools()
        except Exception:
            return tool_name
        for item in tools:
            name = ((item or {}).get("function") or {}).get("name")
            if name in names:
                return name
        return tool_name

    async def _get_speak_handler(self, device_id: str):
        handler = await self.ws_server.get_admin_connection(device_id)
        if not handler:
            raise web.HTTPNotFound(text="device is not online")
        tts = getattr(handler, "tts", None)
        if not tts or not getattr(tts, "tts_text_queue", None):
            raise web.HTTPConflict(text="device TTS is not ready")
        if not getattr(handler, "websocket", None):
            raise web.HTTPConflict(text="device websocket is not ready")
        return handler

    def _speak_busy(self, handler: Any) -> bool:
        if getattr(handler, "client_is_speaking", False):
            return True
        tts = getattr(handler, "tts", None)
        for attr in ("tts_text_queue", "tts_audio_queue"):
            queue = getattr(tts, attr, None)
            if queue and hasattr(queue, "qsize") and queue.qsize() > 0:
                return True
        return False

    async def _wait_until_speak_slot(self, handler: Any):
        deadline = time.time() + self.speak_wait_timeout
        while self._speak_busy(handler):
            if time.time() > deadline:
                raise TimeoutError("timed out waiting for current speech to finish")
            await asyncio.sleep(0.2)

    async def _start_speak_text(self, handler: Any, text: str) -> str:
        await self._wait_until_speak_slot(handler)
        tts = getattr(handler, "tts", None)
        if not tts or not getattr(tts, "tts_text_queue", None):
            raise RuntimeError("device TTS is not ready")

        sentence_id = uuid.uuid4().hex
        handler.sentence_id = sentence_id
        handler.client_abort = False
        await send_tts_message(handler, "start")
        handler.client_is_speaking = True
        if hasattr(tts, "store_tts_text"):
            tts.store_tts_text(sentence_id, text)
        tts.tts_text_queue.put(
            TTSMessageDTO(
                sentence_id=sentence_id,
                sentence_type=SentenceType.FIRST,
                content_type=ContentType.ACTION,
            )
        )
        # Keep admin-triggered speech as one TTS item. The upstream helper
        # splits on punctuation, which makes short reminders choppy.
        if getattr(tts, "_xiaoli_allow_sentence_split", False) and hasattr(tts, "tts_one_sentence"):
            tts.tts_one_sentence(handler, ContentType.TEXT, content_detail=text, sentence_id=sentence_id)
        else:
            tts.tts_text_queue.put(
                TTSMessageDTO(
                    sentence_id=sentence_id,
                    sentence_type=SentenceType.MIDDLE,
                    content_type=ContentType.TEXT,
                    content_detail=text,
                )
            )
        tts.tts_text_queue.put(
            TTSMessageDTO(
                sentence_id=sentence_id,
                sentence_type=SentenceType.LAST,
                content_type=ContentType.ACTION,
            )
        )
        return sentence_id

    def _try_json(self, value: Any) -> Any:
        if not isinstance(value, str):
            return value
        try:
            return json.loads(value)
        except Exception:
            return value
