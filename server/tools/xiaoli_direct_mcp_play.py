#!/usr/bin/env python3
from __future__ import annotations

import argparse
import base64
import hashlib
import http.server
import json
import socket
import struct
import threading
import time
import urllib.parse
from pathlib import Path
from typing import Any


DISCOVERY_MAGIC = "xiaoli-hwtest-discover-v1"
WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


def local_ip() -> str:
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        sock.connect(("8.8.8.8", 80))
        return sock.getsockname()[0]
    finally:
        sock.close()


class DirectPlayState:
    def __init__(self, host: str, http_port: int, ws_port: int, device_id: str, audio_path: Path):
        self.host = host
        self.http_port = http_port
        self.ws_port = ws_port
        self.device_id = device_id
        self.audio_path = audio_path.expanduser().resolve()
        self.audio_route = "/" + urllib.parse.quote(self.audio_path.name)
        self.audio_url = f"http://{host}:{http_port}{self.audio_route}"
        self.done = threading.Event()
        self.result: dict[str, Any] | None = None
        self.error: str | None = None


class HardwareTestHTTPHandler(http.server.BaseHTTPRequestHandler):
    state: DirectPlayState

    def log_message(self, fmt: str, *args: Any) -> None:
        print("http:", fmt % args, flush=True)

    def do_GET(self) -> None:
        self._handle()

    def do_POST(self) -> None:
        self._handle()

    def _handle(self) -> None:
        path = urllib.parse.urlparse(self.path).path
        if path == "/xiaozhi/ota/":
            self._serve_ota()
            return
        if path == self.state.audio_route:
            self._serve_audio()
            return
        self.send_error(404)

    def _serve_ota(self) -> None:
        length = int(self.headers.get("Content-Length", "0") or "0")
        if length:
            self.rfile.read(length)
        payload = {
            "server_time": {
                "timestamp": int(time.time() * 1000),
                "timezone_offset": 480,
            },
            "websocket": {
                "url": f"ws://{self.state.host}:{self.state.ws_port}/xiaozhi/v1/",
                "token": "",
                "version": 1,
            },
            "firmware": {
                "version": "0.0.0",
                "url": "",
            },
        }
        body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _serve_audio(self) -> None:
        body = self.state.audio_path.read_bytes()
        self.send_response(200)
        self.send_header("Content-Type", "audio/ogg")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def start_http_server(state: DirectPlayState) -> http.server.ThreadingHTTPServer:
    HardwareTestHTTPHandler.state = state
    server = http.server.ThreadingHTTPServer(("0.0.0.0", state.http_port), HardwareTestHTTPHandler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def start_discovery(state: DirectPlayState, port: int) -> socket.socket:
    payload = json.dumps(
        {
            "service": "xiaoli-hwtest",
            "version": 1,
            "ota_url": f"http://{state.host}:{state.http_port}/xiaozhi/ota/",
        },
        separators=(",", ":"),
    ).encode("utf-8")
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind(("0.0.0.0", port))

    def loop() -> None:
        while not state.done.is_set():
            try:
                sock.settimeout(0.5)
                data, address = sock.recvfrom(512)
            except OSError:
                continue
            if data.decode("utf-8", errors="ignore").strip() == DISCOVERY_MAGIC:
                sock.sendto(payload, address)
                print(f"discovery: replied to {address[0]}:{address[1]}", flush=True)

    threading.Thread(target=loop, daemon=True).start()
    return sock


def read_exact(conn: socket.socket, size: int) -> bytes:
    chunks = bytearray()
    while len(chunks) < size:
        data = conn.recv(size - len(chunks))
        if not data:
            raise ConnectionError("socket closed")
        chunks.extend(data)
    return bytes(chunks)


def read_ws_frame(conn: socket.socket) -> tuple[int, bytes]:
    first, second = read_exact(conn, 2)
    opcode = first & 0x0F
    masked = (second & 0x80) != 0
    length = second & 0x7F
    if length == 126:
        length = struct.unpack("!H", read_exact(conn, 2))[0]
    elif length == 127:
        length = struct.unpack("!Q", read_exact(conn, 8))[0]
    mask = read_exact(conn, 4) if masked else b""
    payload = bytearray(read_exact(conn, length))
    if masked:
        for i, value in enumerate(payload):
            payload[i] = value ^ mask[i % 4]
    return opcode, bytes(payload)


def send_ws_text(conn: socket.socket, payload: dict[str, Any]) -> None:
    body = json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    header = bytearray([0x81])
    if len(body) < 126:
        header.append(len(body))
    elif len(body) <= 0xFFFF:
        header.append(126)
        header.extend(struct.pack("!H", len(body)))
    else:
        header.append(127)
        header.extend(struct.pack("!Q", len(body)))
    conn.sendall(bytes(header) + body)


def parse_headers(raw: bytes) -> dict[str, str]:
    text = raw.decode("utf-8", errors="replace")
    headers: dict[str, str] = {}
    for line in text.split("\r\n")[1:]:
        if ":" in line:
            key, value = line.split(":", 1)
            headers[key.strip().lower()] = value.strip()
    return headers


def handle_ws_client(conn: socket.socket, state: DirectPlayState) -> None:
    raw = b""
    while b"\r\n\r\n" not in raw:
        raw += conn.recv(4096)
        if not raw:
            raise ConnectionError("empty websocket handshake")
    headers = parse_headers(raw)
    device_id = headers.get("device-id", "")
    if state.device_id and device_id and device_id.lower() != state.device_id.lower():
        state.error = f"unexpected device id {device_id}"
        state.done.set()
        return
    key = headers.get("sec-websocket-key", "")
    accept = base64.b64encode(hashlib.sha1((key + WS_GUID).encode("ascii")).digest()).decode("ascii")
    conn.sendall(
        (
            "HTTP/1.1 101 Switching Protocols\r\n"
            "Upgrade: websocket\r\n"
            "Connection: Upgrade\r\n"
            f"Sec-WebSocket-Accept: {accept}\r\n"
            "\r\n"
        ).encode("ascii")
    )
    print(f"websocket: connected device={device_id or '(unknown)'}", flush=True)

    session_id = "xiaoli-direct-hwtest"
    while not state.done.is_set():
        opcode, payload = read_ws_frame(conn)
        if opcode == 8:
            raise ConnectionError("websocket closed")
        if opcode == 9:
            conn.sendall(b"\x8a\x00")
            continue
        if opcode != 1:
            continue
        message = json.loads(payload.decode("utf-8"))
        print(f"websocket <- {message.get('type')}", flush=True)
        if message.get("type") == "hello":
            send_ws_text(
                conn,
                {
                    "type": "hello",
                    "transport": "websocket",
                    "session_id": session_id,
                    "audio_params": {
                        "format": "opus",
                        "sample_rate": 16000,
                        "channels": 1,
                        "frame_duration": 60,
                    },
                },
            )
            time.sleep(0.3)
            send_ws_text(
                conn,
                {
                    "session_id": session_id,
                    "type": "mcp",
                    "payload": {
                        "jsonrpc": "2.0",
                        "id": 1,
                        "method": "tools/call",
                        "params": {
                            "name": "self.audio_speaker.play_ogg_url",
                            "arguments": {"url": state.audio_url},
                        },
                    },
                },
            )
            print(f"websocket -> tools/call play_ogg_url {state.audio_url}", flush=True)
        elif message.get("type") == "mcp":
            payload_obj = message.get("payload") or {}
            if payload_obj.get("id") == 1:
                if "error" in payload_obj:
                    state.error = json.dumps(payload_obj["error"], ensure_ascii=False)
                else:
                    state.result = payload_obj
                state.done.set()


def start_ws_server(state: DirectPlayState) -> socket.socket:
    server_sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server_sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    server_sock.bind(("0.0.0.0", state.ws_port))
    server_sock.listen(1)

    def loop() -> None:
        while not state.done.is_set():
            try:
                server_sock.settimeout(0.5)
                conn, _ = server_sock.accept()
            except OSError:
                continue
            with conn:
                conn.settimeout(30)
                try:
                    handle_ws_client(conn, state)
                except Exception as exc:
                    if not state.done.is_set():
                        print(f"websocket: {exc}", flush=True)

    threading.Thread(target=loop, daemon=True).start()
    return server_sock


def main() -> int:
    parser = argparse.ArgumentParser(description="Directly play a local Ogg Opus file via minimal Xiaoli MCP server")
    parser.add_argument("--file", required=True)
    parser.add_argument("--device-id", default="")
    parser.add_argument("--host", default="")
    parser.add_argument("--http-port", type=int, default=8080)
    parser.add_argument("--ws-port", type=int, default=8099)
    parser.add_argument("--discovery-port", type=int, default=8989)
    parser.add_argument("--timeout", type=float, default=90)
    args = parser.parse_args()

    host = args.host or local_ip()
    state = DirectPlayState(host, args.http_port, args.ws_port, args.device_id, Path(args.file))
    if not state.audio_path.is_file():
        raise SystemExit(f"audio file not found: {state.audio_path}")

    http_server = start_http_server(state)
    discovery_sock = start_discovery(state, args.discovery_port)
    ws_sock = start_ws_server(state)
    print(f"ota: http://{host}:{args.http_port}/xiaozhi/ota/", flush=True)
    print(f"audio: {state.audio_url}", flush=True)
    print(f"websocket: ws://{host}:{args.ws_port}/xiaozhi/v1/", flush=True)
    print("ready: reset or reboot the ESP32 now", flush=True)

    try:
        if not state.done.wait(args.timeout):
            state.error = "timed out waiting for MCP playback result"
        if state.error:
            print(f"error: {state.error}", flush=True)
            return 1
        print(json.dumps({"ok": True, "result": state.result}, ensure_ascii=False), flush=True)
        return 0
    finally:
        state.done.set()
        discovery_sock.close()
        ws_sock.close()
        http_server.shutdown()
        http_server.server_close()


if __name__ == "__main__":
    raise SystemExit(main())
