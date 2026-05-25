from pathlib import Path


MCP_HANDLER_PATH = Path("/opt/xiaozhi-esp32-server/core/providers/tools/device_mcp/mcp_handler.py")
TEXT_PROCESSOR_PATH = Path("/opt/xiaozhi-esp32-server/core/handle/textMessageProcessor.py")


def patch_mcp_handler_source(text: str) -> str:
    if "xiaoli/vision_frame" in text and "xiaoli_bridge" in text:
        return text
    old = '''    elif "method" in payload:
        method = payload["method"]
        logger.bind(tag=TAG).info(f"收到MCP客户端请求: {method}")

'''
    new = '''    elif "method" in payload:
        method = payload["method"]
        if method == "xiaoli/vision_frame":
            params = payload.get("params") or {}
            stream_bridge = getattr(getattr(conn, "server", None), "xiaoli_bridge", None)
            device_id = getattr(conn, "device_id", None) or getattr(conn, "headers", {}).get("device-id", "")
            frame_data = params.get("data", "")
            if stream_bridge and device_id and isinstance(frame_data, str) and frame_data:
                stream_bridge.publish_stream_frame_base64(
                    device_id,
                    params.get("mime_type", "image/jpeg"),
                    frame_data,
                    {
                        "stream_id": params.get("stream_id", ""),
                        "seq": params.get("seq", ""),
                        "timestamp_ms": params.get("timestamp_ms", ""),
                    },
                )
            return
        logger.bind(tag=TAG).info(f"收到MCP客户端请求: {method}")

'''
    if old not in text:
        raise RuntimeError("Could not find MCP method handler block to patch")
    return text.replace(old, new, 1)


def patch_mcp_handler():
    text = MCP_HANDLER_PATH.read_text(encoding="utf-8")
    MCP_HANDLER_PATH.write_text(patch_mcp_handler_source(text), encoding="utf-8")


def patch_text_message_processor():
    text = TEXT_PROCESSOR_PATH.read_text(encoding="utf-8")
    if "收到mcp视觉帧消息" in text:
        return

    old = '''                # 记录日志
                conn.logger.bind(tag=TAG).info(f"收到{message_type}消息：{message}")

'''
    new = '''                # 记录日志，视觉帧包含大块 base64，只记录摘要，避免刷爆日志。
                if (
                    message_type == "mcp"
                    and isinstance(msg_json.get("payload"), dict)
                    and msg_json["payload"].get("method") == "xiaoli/vision_frame"
                ):
                    params = msg_json["payload"].get("params") or {}
                    conn.logger.bind(tag=TAG).info(
                        f"收到mcp视觉帧消息：stream_id={params.get('stream_id', '')} "
                        f"seq={params.get('seq', '')} base64_size={len(str(params.get('data', '')))}"
                    )
                else:
                    conn.logger.bind(tag=TAG).info(f"收到{message_type}消息：{message}")

'''
    if old not in text:
        raise RuntimeError("Could not find text message logging block to patch")
    TEXT_PROCESSOR_PATH.write_text(text.replace(old, new), encoding="utf-8")


if __name__ == "__main__":
    patch_mcp_handler()
    patch_text_message_processor()
