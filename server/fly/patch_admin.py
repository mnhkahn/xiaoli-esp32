from pathlib import Path


PROJECT_DIR = Path("/opt/xiaozhi-esp32-server")
APP_PATH = PROJECT_DIR / "app.py"
WEBSOCKET_PATH = PROJECT_DIR / "core" / "websocket_server.py"


def patch_app_py():
    text = APP_PATH.read_text(encoding="utf-8")
    if "from xiaoli_admin import XiaoliAdminServer" not in text:
        text = text.replace(
            "from core.websocket_server import WebSocketServer\n",
            "from core.websocket_server import WebSocketServer\nfrom xiaoli_admin import XiaoliAdminServer\n",
        )

    old = "    ota_server = SimpleHttpServer(config)\n    ota_task = asyncio.create_task(ota_server.start())\n"
    new = (
        "    ota_server = SimpleHttpServer(config)\n"
        "    ota_task = asyncio.create_task(ota_server.start())\n"
        "    admin_server = XiaoliAdminServer(config, ws_server)\n"
        "    admin_task = asyncio.create_task(admin_server.start()) if admin_server.enabled else None\n"
    )
    if old in text and "admin_server = XiaoliAdminServer(config, ws_server)" not in text:
        text = text.replace(old, new)

    old_cancel = "        if ota_task:\n            ota_task.cancel()\n"
    new_cancel = (
        "        if ota_task:\n"
        "            ota_task.cancel()\n"
        "        if admin_task:\n"
        "            admin_task.cancel()\n"
    )
    if old_cancel in text and "if admin_task:" not in text:
        text = text.replace(old_cancel, new_cancel)

    old_wait = (
        "        await asyncio.wait(\n"
        "            [stdin_task, ws_task, ota_task] if ota_task else [stdin_task, ws_task],\n"
        "            timeout=3.0,\n"
        "            return_when=asyncio.ALL_COMPLETED,\n"
        "        )\n"
    )
    new_wait = (
        "        tasks = [stdin_task, ws_task]\n"
        "        if ota_task:\n"
        "            tasks.append(ota_task)\n"
        "        if admin_task:\n"
        "            tasks.append(admin_task)\n"
        "        await asyncio.wait(\n"
        "            tasks,\n"
        "            timeout=3.0,\n"
        "            return_when=asyncio.ALL_COMPLETED,\n"
        "        )\n"
    )
    if old_wait in text and "tasks = [stdin_task, ws_task]" not in text:
        text = text.replace(old_wait, new_wait)

    APP_PATH.write_text(text, encoding="utf-8")


def patch_websocket_server_py():
    text = WEBSOCKET_PATH.read_text(encoding="utf-8")

    old_init = (
        "        self.auth = AuthManager(secret_key=secret_key, expire_seconds=expire_seconds)\n"
    )
    new_init = (
        "        self.auth = AuthManager(secret_key=secret_key, expire_seconds=expire_seconds)\n"
        "        self.active_connections = {}\n"
        "        self.active_connections_lock = asyncio.Lock()\n"
    )
    if old_init in text and "self.active_connections = {}" not in text:
        text = text.replace(old_init, new_init)

    marker = "    async def start(self):\n"
    methods = '''    async def register_connection(self, handler):
        device_id = getattr(handler, "device_id", None)
        if not device_id:
            return
        async with self.active_connections_lock:
            self.active_connections[device_id] = handler
        self.logger.bind(tag=TAG).info(f"设备已注册到管理员连接表: {device_id}")

    async def unregister_connection(self, handler):
        device_id = getattr(handler, "device_id", None)
        if not device_id:
            return
        async with self.active_connections_lock:
            if self.active_connections.get(device_id) is handler:
                self.active_connections.pop(device_id, None)
        self.logger.bind(tag=TAG).info(f"设备已从管理员连接表移除: {device_id}")

    async def get_admin_connection(self, device_id):
        async with self.active_connections_lock:
            return self.active_connections.get(device_id)

    async def list_admin_devices(self):
        async with self.active_connections_lock:
            items = list(self.active_connections.items())
        devices = []
        for device_id, handler in items:
            mcp_client = getattr(handler, "mcp_client", None)
            mcp_ready = False
            tool_count = 0
            if mcp_client:
                try:
                    mcp_ready = await mcp_client.is_ready()
                    tool_count = len(mcp_client.tools)
                except Exception:
                    mcp_ready = False
            devices.append(
                {
                    "device_id": device_id,
                    "session_id": getattr(handler, "session_id", ""),
                    "client_ip": getattr(handler, "client_ip", ""),
                    "mcp_ready": mcp_ready,
                    "tool_count": tool_count,
                    "connected_at": getattr(handler, "first_activity_time", 0),
                    "last_activity": getattr(handler, "last_activity_time", 0),
                }
            )
        devices.sort(key=lambda item: item["device_id"])
        return devices

'''
    if marker in text and "async def register_connection" not in text:
        text = text.replace(marker, methods + marker)

    old_handler = '''        handler = ConnectionHandler(
            self.config,
            self._vad,
            self._asr,
            self._llm,
            self._memory,
            self._intent,
            self,  # 传入server实例
        )
        try:
            await handler.handle_connection(websocket)
        except Exception as e:
            self.logger.bind(tag=TAG).error(f"处理连接时出错: {e}")
        finally:
            # 强制关闭连接（如果还没有关闭的话）
'''
    new_handler = '''        handler = ConnectionHandler(
            self.config,
            self._vad,
            self._asr,
            self._llm,
            self._memory,
            self._intent,
            self,  # 传入server实例
        )
        handler.headers = headers
        handler.device_id = headers.get("device-id", None)
        await self.register_connection(handler)
        try:
            await handler.handle_connection(websocket)
        except Exception as e:
            self.logger.bind(tag=TAG).error(f"处理连接时出错: {e}")
        finally:
            await self.unregister_connection(handler)
            # 强制关闭连接（如果还没有关闭的话）
'''
    if old_handler in text and "await self.register_connection(handler)" not in text:
        text = text.replace(old_handler, new_handler)

    WEBSOCKET_PATH.write_text(text, encoding="utf-8")


def main():
    patch_app_py()
    patch_websocket_server_py()


if __name__ == "__main__":
    main()
