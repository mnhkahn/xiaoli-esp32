#!/usr/bin/env python3
from __future__ import annotations

import argparse
import functools
import http.server
import json
import os
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any


DISCOVERY_MAGIC = "xiaoli-hwtest-discover-v1"
DISCOVERY_PORT = 8989
DEFAULT_PUBLIC_PORT = 8080
DEFAULT_BRIDGE_PORT = 8005
DEFAULT_ADMIN_PORT = 8004
DEFAULT_BRIDGE_BASE_URL = "http://127.0.0.1:8005"
DEFAULT_INTERNAL_TOKEN = "xiaoli-hwtest-local-internal-token"
DEFAULT_AUTH_KEY = "0123456789abcdef0123456789abcdef"


def local_ip() -> str:
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        sock.connect(("8.8.8.8", 80))
        return sock.getsockname()[0]
    except OSError:
        return socket.gethostbyname(socket.gethostname())
    finally:
        sock.close()


def discovery_payload(host: str, public_port: int, token: str = "") -> bytes:
    payload = {
        "service": "xiaoli-hwtest",
        "version": 1,
        "ota_url": f"http://{host}:{public_port}/xiaozhi/ota/",
        "token": token,
    }
    return json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode("utf-8")


def build_docker_command(
    image: str,
    host: str,
    public_port: int,
    bridge_port: int,
    admin_port: int,
    device_ids: str,
    auth_key: str,
    internal_token: str,
) -> list[str]:
    env = {
        "PUBLIC_BASE_URL": f"http://{host}:{public_port}",
        "PUBLIC_WS_URL": f"ws://{host}:{public_port}/xiaozhi/v1/",
        "PUBLIC_VISION_URL": f"http://{host}:{public_port}/mcp/vision/explain",
        "ENABLE_SERVER_AUTH": "true",
        "SERVER_AUTH_KEY": auth_key,
        "ALLOWED_DEVICE_IDS": device_ids,
        "XIAOLI_ADMIN_ENABLED": "true",
        "XIAOLI_ADMIN_PORT": str(admin_port),
        "ADMIN_PUBLIC_BASE_URL": f"http://{host}:{public_port}",
        "ADMIN_SESSION_SECRET": internal_token,
        "XIAOLI_ADMIN_INTERNAL_TOKEN": internal_token,
        "XIAOLI_BRIDGE_HOST": "0.0.0.0",
        "XIAOLI_BRIDGE_PORT": str(bridge_port),
        "XIAOLI_GO_ADMIN_BASE_URL": f"http://127.0.0.1:{admin_port}",
    }
    command = [
        "docker",
        "run",
        "--rm",
        "--name",
        "xiaoli-hwtest",
        "-p",
        f"{public_port}:8080",
        "-p",
        f"{bridge_port}:{bridge_port}",
        "-p",
        f"{admin_port}:{admin_port}",
    ]
    for key, value in env.items():
        command.extend(["-e", f"{key}={value}"])
    command.append(image)
    return command


class DiscoveryResponder:
    def __init__(self, host: str, public_port: int, port: int = DISCOVERY_PORT, token: str = ""):
        self.host = host
        self.public_port = public_port
        self.port = port
        self.token = token
        self._sock: socket.socket | None = None

    def serve_forever(self) -> None:
        payload = discovery_payload(self.host, self.public_port, self.token)
        sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        sock.bind(("0.0.0.0", self.port))
        self._sock = sock
        print(
            f"Discovery listening on udp://0.0.0.0:{self.port}, advertising http://{self.host}:{self.public_port}/xiaozhi/ota/",
            flush=True,
        )
        while True:
            data, address = sock.recvfrom(512)
            if data.decode("utf-8", errors="ignore").strip() == DISCOVERY_MAGIC:
                sock.sendto(payload, address)

    def close(self) -> None:
        if self._sock is not None:
            self._sock.close()


def request_json(method: str, url: str, payload: Any | None = None, timeout: int = 120) -> Any:
    data = None
    headers = {"Accept": "application/json"}
    if payload is not None:
        data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            body = response.read()
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")
        raise RuntimeError(f"{method} {url} failed: {exc.code} {body}") from exc
    return json.loads(body.decode("utf-8"))


def bridge_call(base_url: str, device_id: str, tool: str, arguments: dict[str, Any], timeout: int = 120) -> Any:
    return request_json(
        "POST",
        base_url.rstrip("/") + "/bridge/call",
        {
            "device_id": device_id,
            "tool": tool,
            "arguments": arguments,
            "timeout": timeout,
        },
        timeout=timeout + 5,
    )


def latest_image(public_base_url: str, internal_token: str, device_id: str, wait_seconds: float = 5) -> bytes:
    deadline = time.time() + wait_seconds
    query = urllib.parse.urlencode({"device_id": device_id})
    url = public_base_url.rstrip("/") + "/admin/internal/images/latest?" + query
    headers = {"X-Xiaoli-Internal-Token": internal_token}
    while True:
        req = urllib.request.Request(url, headers=headers, method="GET")
        try:
            with urllib.request.urlopen(req, timeout=5) as response:
                return response.read()
        except urllib.error.HTTPError as exc:
            if exc.code != 404 or time.time() >= deadline:
                body = exc.read().decode("utf-8", errors="replace")
                raise RuntimeError(f"GET {url} failed: {exc.code} {body}") from exc
        if time.time() >= deadline:
            raise TimeoutError("timed out waiting for latest snapshot image")
        time.sleep(0.25)


def local_file_url(path: Path, host: str, port: int) -> tuple[Path, str]:
    audio_file = path.expanduser().resolve()
    if not audio_file.is_file():
        raise FileNotFoundError(f"audio file not found: {audio_file}")
    return audio_file.parent, f"http://{host}:{port}/{urllib.parse.quote(audio_file.name)}"


def start_local_file_server(directory: Path, port: int) -> http.server.ThreadingHTTPServer:
    handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=str(directory))
    server = http.server.ThreadingHTTPServer(("0.0.0.0", port), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server


def print_json(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, indent=2))


def cmd_serve(args: argparse.Namespace) -> int:
    host = args.host or local_ip()
    child: subprocess.Popen | None = None
    if not args.no_docker:
        if not args.device_ids:
            raise SystemExit("--device-id or --device-ids is required when starting the local server container")
        command = build_docker_command(
            image=args.image,
            host=host,
            public_port=args.public_port,
            bridge_port=args.bridge_port,
            admin_port=args.admin_port,
            device_ids=args.device_ids,
            auth_key=args.auth_key,
            internal_token=args.internal_token,
        )
        print("Starting local server container:", " ".join(command), flush=True)
        child = subprocess.Popen(command)

    responder = DiscoveryResponder(host, args.public_port, args.discovery_port, args.discovery_token)

    def stop(signum, frame):
        responder.close()
        if child and child.poll() is None:
            child.terminate()
        raise SystemExit(128 + signum)

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    responder.serve_forever()
    return 0


def cmd_devices(args: argparse.Namespace) -> int:
    print_json(request_json("GET", args.bridge_base_url.rstrip("/") + "/bridge/devices", timeout=10))
    return 0


def cmd_tools(args: argparse.Namespace) -> int:
    query = urllib.parse.urlencode({"device_id": args.device_id})
    print_json(request_json("GET", args.bridge_base_url.rstrip("/") + "/bridge/tools?" + query, timeout=10))
    return 0


def cmd_call(args: argparse.Namespace) -> int:
    arguments = json.loads(args.arguments)
    print_json(bridge_call(args.bridge_base_url, args.device_id, args.tool, arguments, args.timeout))
    return 0


def cmd_snapshot(args: argparse.Namespace) -> int:
    result = bridge_call(
        args.bridge_base_url,
        args.device_id,
        "self.camera.snapshot",
        {"resolution": args.resolution},
        args.timeout,
    )
    print_json(result)
    if args.out:
        image = latest_image(args.public_base_url, args.internal_token, args.device_id)
        Path(args.out).parent.mkdir(parents=True, exist_ok=True)
        Path(args.out).write_bytes(image)
        print(f"Wrote snapshot: {args.out}", file=sys.stderr)
    return 0


def cmd_play_sound(args: argparse.Namespace) -> int:
    print_json(
        bridge_call(
            args.bridge_base_url,
            args.device_id,
            "self.audio_speaker.play_sound",
            {"name": args.name},
            args.timeout,
        )
    )
    return 0


def cmd_play_ogg_url(args: argparse.Namespace) -> int:
    print_json(
        bridge_call(
            args.bridge_base_url,
            args.device_id,
            "self.audio_speaker.play_ogg_url",
            {"url": args.url},
            args.timeout,
        )
    )
    return 0


def cmd_play_file(args: argparse.Namespace) -> int:
    host = args.host or local_ip()
    directory, url = local_file_url(Path(args.file), host, args.file_port)
    server = start_local_file_server(directory, args.file_port)
    print(f"Serving {args.file} as {url}", file=sys.stderr, flush=True)
    try:
        print_json(
            bridge_call(
                args.bridge_base_url,
                args.device_id,
                "self.audio_speaker.play_ogg_url",
                {"url": url},
                args.timeout,
            )
        )
        if args.keepalive_seconds > 0:
            time.sleep(args.keepalive_seconds)
    finally:
        server.shutdown()
        server.server_close()
    return 0


def cmd_speak(args: argparse.Namespace) -> int:
    print_json(
        request_json(
            "POST",
            args.bridge_base_url.rstrip("/") + "/bridge/speak",
            {"device_id": args.device_id, "text": args.text},
            timeout=args.timeout,
        )
    )
    return 0


def add_bridge_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--bridge-base-url", default=os.environ.get("XIAOLI_HWTEST_BRIDGE_URL", DEFAULT_BRIDGE_BASE_URL))


def add_device_arg(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--device-id", required=True)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Local hardware test helper for xiaoli ESP32 devices")
    sub = parser.add_subparsers(dest="command", required=True)

    serve = sub.add_parser("serve", help="start discovery and optionally the local server container")
    serve.add_argument("--host", default=os.environ.get("XIAOLI_HWTEST_HOST", ""))
    serve.add_argument("--image", default=os.environ.get("XIAOLI_HWTEST_IMAGE", "xiaoli-server:hwtest"))
    serve.add_argument("--device-id", dest="device_ids", default=os.environ.get("ALLOWED_DEVICE_IDS", ""))
    serve.add_argument("--device-ids", dest="device_ids")
    serve.add_argument("--public-port", type=int, default=DEFAULT_PUBLIC_PORT)
    serve.add_argument("--bridge-port", type=int, default=DEFAULT_BRIDGE_PORT)
    serve.add_argument("--admin-port", type=int, default=DEFAULT_ADMIN_PORT)
    serve.add_argument("--discovery-port", type=int, default=DISCOVERY_PORT)
    serve.add_argument("--discovery-token", default="")
    serve.add_argument("--auth-key", default=os.environ.get("SERVER_AUTH_KEY", DEFAULT_AUTH_KEY))
    serve.add_argument("--internal-token", default=os.environ.get("XIAOLI_ADMIN_INTERNAL_TOKEN", DEFAULT_INTERNAL_TOKEN))
    serve.add_argument("--no-docker", action="store_true", help="only run UDP discovery responder")
    serve.set_defaults(func=cmd_serve)

    devices = sub.add_parser("devices", help="list devices on the local bridge")
    add_bridge_args(devices)
    devices.set_defaults(func=cmd_devices)

    tools = sub.add_parser("tools", help="list MCP tools for a device")
    add_bridge_args(tools)
    add_device_arg(tools)
    tools.set_defaults(func=cmd_tools)

    call = sub.add_parser("call", help="call any MCP tool")
    add_bridge_args(call)
    add_device_arg(call)
    call.add_argument("--tool", required=True)
    call.add_argument("--arguments", default="{}")
    call.add_argument("--timeout", type=int, default=120)
    call.set_defaults(func=cmd_call)

    snapshot = sub.add_parser("snapshot", help="take a camera snapshot and optionally save it")
    add_bridge_args(snapshot)
    add_device_arg(snapshot)
    snapshot.add_argument("--resolution", default="vga")
    snapshot.add_argument("--out", default="")
    snapshot.add_argument("--timeout", type=int, default=120)
    snapshot.add_argument("--public-base-url", default=os.environ.get("XIAOLI_HWTEST_PUBLIC_URL", "http://127.0.0.1:8080"))
    snapshot.add_argument("--internal-token", default=os.environ.get("XIAOLI_ADMIN_INTERNAL_TOKEN", DEFAULT_INTERNAL_TOKEN))
    snapshot.set_defaults(func=cmd_snapshot)

    play_sound = sub.add_parser("play-sound", help="play a built-in prompt sound")
    add_bridge_args(play_sound)
    add_device_arg(play_sound)
    play_sound.add_argument("--name", default="success")
    play_sound.add_argument("--timeout", type=int, default=30)
    play_sound.set_defaults(func=cmd_play_sound)

    play_ogg_url = sub.add_parser("play-ogg-url", help="play an Ogg Opus file from an HTTP URL")
    add_bridge_args(play_ogg_url)
    add_device_arg(play_ogg_url)
    play_ogg_url.add_argument("--url", required=True)
    play_ogg_url.add_argument("--timeout", type=int, default=60)
    play_ogg_url.set_defaults(func=cmd_play_ogg_url)

    play_file = sub.add_parser("play-file", help="serve and play a local Ogg Opus file")
    add_bridge_args(play_file)
    add_device_arg(play_file)
    play_file.add_argument("--file", required=True)
    play_file.add_argument("--host", default=os.environ.get("XIAOLI_HWTEST_HOST", ""))
    play_file.add_argument("--file-port", type=int, default=int(os.environ.get("XIAOLI_HWTEST_FILE_PORT", "9000")))
    play_file.add_argument("--timeout", type=int, default=60)
    play_file.add_argument("--keepalive-seconds", type=float, default=1)
    play_file.set_defaults(func=cmd_play_file)

    speak = sub.add_parser("speak", help="queue text through the server TTS path")
    add_bridge_args(speak)
    add_device_arg(speak)
    speak.add_argument("--text", required=True)
    speak.add_argument("--timeout", type=int, default=120)
    speak.set_defaults(func=cmd_speak)

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
