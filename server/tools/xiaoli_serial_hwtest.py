#!/usr/bin/env python3
from __future__ import annotations

import argparse
import base64
import fcntl
import json
import os
import select
import struct
import sys
import termios
import time
import uuid
from pathlib import Path
from typing import Iterable, Iterator, Any


COMMAND_PREFIX = b"XIAOLI_TEST "
RESULT_PREFIX = "XIAOLI_TEST_RESULT "
DATA_PREFIX = "XIAOLI_TEST_DATA "
END_PREFIX = "XIAOLI_TEST_END "
DEFAULT_BAUD = 115200
DEFAULT_PORT = "/dev/cu.usbserial-14310"
MACOS_IOSSIOSPEED = 0x80085402


def command_line(request_id: str, cmd: str, **kwargs: Any) -> bytes:
    payload = {"id": request_id, "cmd": cmd}
    payload.update(kwargs)
    return COMMAND_PREFIX + json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode("utf-8") + b"\n"


def parse_prefixed_line(line: str) -> tuple[str, dict[str, Any]] | None:
    line = line.strip()
    for kind, prefix in (("result", RESULT_PREFIX), ("data", DATA_PREFIX), ("end", END_PREFIX)):
        prefix_at = line.find(prefix)
        if prefix_at >= 0:
            payload_text = line[prefix_at + len(prefix) :].lstrip()
            try:
                payload, _ = json.JSONDecoder().raw_decode(payload_text)
            except json.JSONDecodeError:
                return None
            if isinstance(payload, dict):
                return kind, payload
    return None


def collect_snapshot(request_id: str, lines: Iterable[str]) -> bytes:
    chunks: list[tuple[int, str]] = []
    saw_result = False
    for line in lines:
        parsed = parse_prefixed_line(line)
        if parsed is None:
            continue
        kind, payload = parsed
        if payload.get("id") != request_id:
            continue
        if kind == "result":
            if not payload.get("ok"):
                raise RuntimeError(str(payload.get("error") or "snapshot failed"))
            saw_result = True
        elif kind == "data":
            chunks.append((int(payload.get("seq", 0)), str(payload.get("data", ""))))
        elif kind == "end":
            if not payload.get("ok"):
                raise RuntimeError(str(payload.get("error") or "snapshot failed"))
            if not saw_result:
                raise RuntimeError("snapshot ended before result header")
            return b"".join(base64.b64decode(data) for _, data in sorted(chunks))
    raise TimeoutError("snapshot response did not finish")


def iter_base64_chunks(data: bytes, chunk_bytes: int = 768) -> Iterator[str]:
    for offset in range(0, len(data), chunk_bytes):
        yield base64.b64encode(data[offset : offset + chunk_bytes]).decode("ascii")


class SerialPort:
    def __init__(self, path: str, baud: int = DEFAULT_BAUD):
        self.path = path
        self.fd = os.open(path, os.O_RDWR | os.O_NOCTTY | os.O_NONBLOCK)
        self._old_attrs = termios.tcgetattr(self.fd)
        attrs = termios.tcgetattr(self.fd)
        attrs[0] = 0
        attrs[1] = 0
        attrs[2] = attrs[2] | termios.CLOCAL | termios.CREAD
        attrs[2] = attrs[2] & ~termios.CSIZE
        attrs[2] = attrs[2] | termios.CS8
        attrs[2] = attrs[2] & ~(termios.PARENB | termios.CSTOPB | termios.CRTSCTS)
        attrs[3] = 0
        speed = self._baud_constant(baud)
        custom_baud = speed is None
        if custom_baud:
            speed = termios.B9600
        attrs[4] = speed
        attrs[5] = speed
        termios.tcsetattr(self.fd, termios.TCSANOW, attrs)
        if custom_baud:
            self._set_custom_baud(baud)
        self._buffer = bytearray()

    def _baud_constant(self, baud: int) -> int | None:
        name = "B" + str(baud)
        if hasattr(termios, name):
            return getattr(termios, name)
        if sys.platform == "darwin":
            return None
        raise ValueError(f"unsupported baud rate: {baud}")

    def _set_custom_baud(self, baud: int) -> None:
        if sys.platform != "darwin":
            raise ValueError(f"unsupported baud rate: {baud}")
        fcntl.ioctl(self.fd, MACOS_IOSSIOSPEED, struct.pack("Q", baud))

    def close(self) -> None:
        try:
            termios.tcsetattr(self.fd, termios.TCSANOW, self._old_attrs)
        finally:
            os.close(self.fd)

    def __enter__(self) -> "SerialPort":
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self.close()

    def _write_all(self, data: bytes) -> None:
        view = memoryview(data)
        offset = 0
        while offset < len(view):
            try:
                written = os.write(self.fd, view[offset:])
            except BlockingIOError:
                select.select([], [self.fd], [], 0.2)
                continue
            if written <= 0:
                raise RuntimeError("serial write returned no progress")
            offset += written

    def clear_input_line(self, drain_seconds: float = 0.2) -> None:
        self._write_all(b"\n")
        termios.tcdrain(self.fd)
        deadline = time.monotonic() + drain_seconds
        while time.monotonic() < deadline:
            if self.read_line(max(0.0, deadline - time.monotonic())) is None:
                break

    def write_line(self, line: bytes) -> None:
        self._write_all(line)
        termios.tcdrain(self.fd)

    def read_line(self, timeout: float) -> str | None:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if b"\n" in self._buffer:
                raw, _, rest = self._buffer.partition(b"\n")
                self._buffer = bytearray(rest)
                return raw.decode("utf-8", errors="replace").rstrip("\r")
            remaining = max(0.0, deadline - time.monotonic())
            readable, _, _ = select.select([self.fd], [], [], min(0.2, remaining))
            if not readable:
                continue
            try:
                data = os.read(self.fd, 4096)
            except BlockingIOError:
                continue
            if data:
                self._buffer.extend(data)
        return None


class HwtestClient:
    def __init__(self, serial_port: SerialPort, verbose: bool = False):
        self.serial = serial_port
        self.verbose = verbose

    def request_id(self) -> str:
        return uuid.uuid4().hex[:12]

    def send(self, request_id: str, cmd: str, **kwargs: Any) -> None:
        self.serial.clear_input_line()
        self.serial.write_line(command_line(request_id, cmd, **kwargs))

    def events_until(self, request_id: str, timeout: float) -> Iterator[tuple[str, dict[str, Any]]]:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            line = self.serial.read_line(max(0.0, deadline - time.monotonic()))
            if line is None:
                break
            parsed = parse_prefixed_line(line)
            if parsed is None:
                if self.verbose:
                    print(line, file=sys.stderr)
                continue
            kind, payload = parsed
            if payload.get("id") == request_id:
                yield kind, payload
                if kind == "end":
                    return
            elif self.verbose:
                print(f"ignored {kind}: {payload}", file=sys.stderr)
        raise TimeoutError(f"timed out waiting for {request_id}")

    def wait_result(self, request_id: str, timeout: float = 10) -> dict[str, Any]:
        for kind, payload in self.events_until(request_id, timeout):
            if kind == "result":
                if not payload.get("ok"):
                    raise RuntimeError(str(payload.get("error") or "command failed"))
                return payload
        raise TimeoutError(f"timed out waiting for result {request_id}")

    def wait_ready(self, timeout: float = 10, probe_timeout: float = 1) -> dict[str, Any]:
        deadline = time.monotonic() + timeout
        request_id = self.request_id()
        self.send(request_id, "ping")
        last_ping: dict[str, Any] | None = None
        last_error: Exception | None = None
        while time.monotonic() < deadline:
            line = self.serial.read_line(max(0.0, deadline - time.monotonic()))
            if line is None:
                break
            parsed = parse_prefixed_line(line)
            if parsed is None:
                if self.verbose:
                    print(line, file=sys.stderr)
                continue
            kind, payload = parsed
            if kind == "result" and payload.get("ok") and payload.get("cmd") == "ping":
                last_ping = payload
                if payload.get("id") == request_id:
                    return payload
                if self.verbose:
                    print(f"ignored {kind}: {payload}", file=sys.stderr)
                continue
            if payload.get("id") == request_id and kind == "result" and not payload.get("ok"):
                last_error = RuntimeError(str(payload.get("error") or "ping failed"))
            elif self.verbose:
                print(f"ignored {kind}: {payload}", file=sys.stderr)
        if last_ping is not None:
            return last_ping
        if last_error is not None:
            raise TimeoutError(f"serial hardware test task is not ready: {last_error}") from last_error
        raise TimeoutError("serial hardware test task is not ready")

    def snapshot(
        self,
        resolution: str,
        timeout: float,
        quality: int | None = None,
        settle_ms: int | None = None,
        discard_frames: int | None = None,
        attempts: int = 3,
        command_timeout: float = 5,
    ) -> tuple[dict[str, Any], bytes]:
        payload: dict[str, Any] = {"resolution": resolution}
        if quality is not None:
            payload["quality"] = quality
        if settle_ms is not None:
            payload["settle_ms"] = settle_ms
        if discard_frames is not None:
            payload["discard_frames"] = discard_frames
        attempts = max(1, attempts)
        last_error: Exception | None = None
        for attempt in range(1, attempts + 1):
            request_id = self.request_id()
            self.send(request_id, "snapshot", **payload)
            try:
                return self.collect_snapshot_response(request_id, timeout, command_timeout)
            except TimeoutError as exc:
                last_error = exc
                if self.verbose and attempt < attempts:
                    print(f"snapshot {request_id} timed out before response; retrying", file=sys.stderr)
        if last_error is not None:
            raise last_error
        raise TimeoutError("snapshot response did not finish")

    def collect_snapshot_response(
        self,
        request_id: str,
        timeout: float,
        command_timeout: float,
    ) -> tuple[dict[str, Any], bytes]:
        deadline = time.monotonic() + timeout
        header_deadline = time.monotonic() + command_timeout
        header: dict[str, Any] | None = None
        chunks: list[tuple[int, str]] = []
        while time.monotonic() < deadline:
            active_deadline = header_deadline if header is None else deadline
            remaining = max(0.0, min(deadline, active_deadline) - time.monotonic())
            line = self.serial.read_line(remaining)
            if line is None:
                if header is None:
                    raise TimeoutError(f"timed out waiting for snapshot header {request_id}")
                break
            parsed = parse_prefixed_line(line)
            if parsed is None:
                if self.verbose:
                    print(line, file=sys.stderr)
                continue
            kind, payload = parsed
            if payload.get("id") != request_id:
                if self.verbose:
                    print(f"ignored {kind}: {payload}", file=sys.stderr)
                continue
            if kind == "result":
                if not payload.get("ok"):
                    raise RuntimeError(str(payload.get("error") or "snapshot failed"))
                header = payload
            elif kind == "data":
                chunks.append((int(payload.get("seq", 0)), str(payload.get("data", ""))))
            elif kind == "end":
                if not payload.get("ok"):
                    raise RuntimeError(str(payload.get("error") or "snapshot failed"))
                if header is None:
                    raise RuntimeError("snapshot ended before result header")
                image = b"".join(base64.b64decode(data) for _, data in sorted(chunks))
                return header, image
        raise TimeoutError(f"snapshot response did not finish {request_id}")

    def play_sound(self, name: str, timeout: float) -> dict[str, Any]:
        request_id = self.request_id()
        self.send(request_id, "play_sound", name=name)
        return self.wait_result(request_id, timeout)

    def status(self, timeout: float) -> dict[str, Any]:
        request_id = self.request_id()
        self.send(request_id, "status")
        return self.wait_result(request_id, timeout)

    def set_volume(self, volume: int, timeout: float) -> dict[str, Any]:
        request_id = self.request_id()
        self.send(request_id, "set_volume", volume=volume)
        return self.wait_result(request_id, timeout)

    def tone(self, frequency: int, duration_ms: int, amplitude: int, timeout: float) -> dict[str, Any]:
        request_id = self.request_id()
        self.send(
            request_id,
            "tone",
            frequency=frequency,
            duration_ms=duration_ms,
            amplitude=amplitude,
        )
        return self.wait_result(request_id, timeout)

    def play_ogg(self, path: Path, timeout: float) -> dict[str, Any]:
        audio = path.read_bytes()
        request_id = self.request_id()
        self.send(request_id, "play_ogg_begin", length=len(audio))
        self.wait_result(request_id, timeout)
        for seq, chunk in enumerate(iter_base64_chunks(audio)):
            self.send(request_id, "play_ogg_chunk", seq=seq, data=chunk)
            self.wait_result(request_id, timeout)
        self.send(request_id, "play_ogg_end")
        return self.wait_result(request_id, timeout)


def add_common_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--port", default=DEFAULT_PORT)
    parser.add_argument("--baud", type=int, default=DEFAULT_BAUD)
    parser.add_argument("--timeout", type=float, default=60)
    parser.add_argument("--verbose", action="store_true")


def cmd_snapshot(args: argparse.Namespace) -> int:
    with SerialPort(args.port, args.baud) as serial_port:
        client = HwtestClient(serial_port, args.verbose)
        client.wait_ready(timeout=min(args.timeout, 10))
        header, image = client.snapshot(
            args.resolution,
            args.timeout,
            quality=args.quality,
            settle_ms=args.settle_ms,
            discard_frames=args.discard_frames,
            attempts=args.attempts,
            command_timeout=args.command_timeout,
        )
    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_bytes(image)
    print(json.dumps({**header, "out": str(out)}, ensure_ascii=False))
    return 0


def cmd_batch_snapshot(args: argparse.Namespace) -> int:
    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    with SerialPort(args.port, args.baud) as serial_port:
        client = HwtestClient(serial_port, args.verbose)
        client.wait_ready(timeout=min(args.timeout, 10))
        for resolution in args.resolutions:
            client.wait_ready(timeout=min(args.timeout, 10))
            header, image = client.snapshot(
                resolution,
                args.timeout,
                quality=args.quality,
                settle_ms=args.settle_ms,
                discard_frames=args.discard_frames,
                attempts=args.attempts,
                command_timeout=args.command_timeout,
            )
            out = out_dir / f"{resolution}.jpg"
            out.write_bytes(image)
            print(json.dumps({**header, "out": str(out)}, ensure_ascii=False))
    return 0


def cmd_timelapse_snapshot(args: argparse.Namespace) -> int:
    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    with SerialPort(args.port, args.baud) as serial_port:
        client = HwtestClient(serial_port, args.verbose)
        client.wait_ready(timeout=min(args.timeout, 10))
        for index in range(1, args.count + 1):
            if index > 1 and args.interval > 0:
                time.sleep(args.interval)
            client.wait_ready(timeout=min(args.timeout, 10))
            header, image = client.snapshot(
                args.resolution,
                args.timeout,
                quality=args.quality,
                settle_ms=args.settle_ms,
                discard_frames=args.discard_frames,
                attempts=args.attempts,
                command_timeout=args.command_timeout,
            )
            out = out_dir / f"{args.prefix}{index:03d}.jpg"
            out.write_bytes(image)
            print(json.dumps({**header, "index": index, "out": str(out)}, ensure_ascii=False), flush=True)
    return 0


def cmd_play_sound(args: argparse.Namespace) -> int:
    with SerialPort(args.port, args.baud) as serial_port:
        client = HwtestClient(serial_port, args.verbose)
        client.wait_ready(timeout=min(args.timeout, 10))
        for index in range(1, args.count + 1):
            result = client.play_sound(args.name, args.timeout)
            print(json.dumps({**result, "index": index}, ensure_ascii=False), flush=True)
            if index < args.count and args.interval > 0:
                time.sleep(args.interval)
    return 0


def cmd_status(args: argparse.Namespace) -> int:
    with SerialPort(args.port, args.baud) as serial_port:
        client = HwtestClient(serial_port, args.verbose)
        client.wait_ready(timeout=min(args.timeout, 10))
        result = client.status(args.timeout)
    print(json.dumps(result, ensure_ascii=False))
    return 0


def cmd_set_volume(args: argparse.Namespace) -> int:
    with SerialPort(args.port, args.baud) as serial_port:
        client = HwtestClient(serial_port, args.verbose)
        client.wait_ready(timeout=min(args.timeout, 10))
        result = client.set_volume(args.volume, args.timeout)
    print(json.dumps(result, ensure_ascii=False))
    return 0


def cmd_tone(args: argparse.Namespace) -> int:
    with SerialPort(args.port, args.baud) as serial_port:
        client = HwtestClient(serial_port, args.verbose)
        client.wait_ready(timeout=min(args.timeout, 10))
        result = client.tone(args.frequency, args.duration_ms, args.amplitude, args.timeout)
    print(json.dumps(result, ensure_ascii=False))
    return 0


def cmd_play_ogg(args: argparse.Namespace) -> int:
    with SerialPort(args.port, args.baud) as serial_port:
        client = HwtestClient(serial_port, args.verbose)
        client.wait_ready(timeout=min(args.timeout, 10))
        result = client.play_ogg(Path(args.file), args.timeout)
    print(json.dumps(result, ensure_ascii=False))
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="USB serial hardware tests for Xiaoli ESP32")
    sub = parser.add_subparsers(dest="command", required=True)

    snapshot = sub.add_parser("snapshot", help="capture a JPEG over USB serial")
    add_common_args(snapshot)
    snapshot.add_argument("--resolution", default="vga", choices=["qvga", "vga", "svga", "xga", "uxga", "legacy_vga"])
    snapshot.add_argument("--quality", type=int, help="ESP camera JPEG quality, 0-63; lower is higher quality")
    snapshot.add_argument("--settle-ms", type=int, help="milliseconds to wait after changing camera profile")
    snapshot.add_argument("--discard-frames", type=int, help="frames to discard before keeping the snapshot")
    snapshot.add_argument("--attempts", type=int, default=3, help="retry snapshot if the board does not acknowledge the command")
    snapshot.add_argument("--command-timeout", type=float, default=5, help="seconds to wait for snapshot header before retrying")
    snapshot.add_argument("--out", required=True)
    snapshot.set_defaults(func=cmd_snapshot)

    batch_snapshot = sub.add_parser("batch-snapshot", help="capture multiple JPEGs over one USB serial session")
    add_common_args(batch_snapshot)
    batch_snapshot.add_argument("--resolutions", nargs="+", default=["qvga", "vga", "svga", "xga", "uxga"],
                                choices=["qvga", "vga", "svga", "xga", "uxga", "legacy_vga"])
    batch_snapshot.add_argument("--quality", type=int, help="ESP camera JPEG quality, 0-63; lower is higher quality")
    batch_snapshot.add_argument("--settle-ms", type=int, help="milliseconds to wait after changing camera profile")
    batch_snapshot.add_argument("--discard-frames", type=int, help="frames to discard before keeping the snapshot")
    batch_snapshot.add_argument("--attempts", type=int, default=3, help="retry each snapshot if the board does not acknowledge the command")
    batch_snapshot.add_argument("--command-timeout", type=float, default=5, help="seconds to wait for snapshot header before retrying")
    batch_snapshot.add_argument("--out-dir", required=True)
    batch_snapshot.set_defaults(func=cmd_batch_snapshot)

    timelapse_snapshot = sub.add_parser("timelapse-snapshot", help="capture repeated JPEGs at a fixed interval")
    add_common_args(timelapse_snapshot)
    timelapse_snapshot.add_argument("--resolution", default="vga", choices=["qvga", "vga", "svga", "xga", "uxga", "legacy_vga"])
    timelapse_snapshot.add_argument("--quality", type=int, help="ESP camera JPEG quality, 0-63; lower is higher quality")
    timelapse_snapshot.add_argument("--settle-ms", type=int, help="milliseconds to wait after changing camera profile")
    timelapse_snapshot.add_argument("--discard-frames", type=int, help="frames to discard before keeping the snapshot")
    timelapse_snapshot.add_argument("--attempts", type=int, default=3, help="retry snapshot if the board does not acknowledge the command")
    timelapse_snapshot.add_argument("--command-timeout", type=float, default=5, help="seconds to wait for snapshot header before retrying")
    timelapse_snapshot.add_argument("--interval", type=float, default=5, help="seconds between captures")
    timelapse_snapshot.add_argument("--count", type=int, default=12, help="number of snapshots to capture")
    timelapse_snapshot.add_argument("--prefix", default="frame_", help="output filename prefix")
    timelapse_snapshot.add_argument("--out-dir", required=True)
    timelapse_snapshot.set_defaults(func=cmd_timelapse_snapshot)

    play_sound = sub.add_parser("play-sound", help="play a built-in prompt sound")
    add_common_args(play_sound)
    play_sound.add_argument("--name", default="success")
    play_sound.add_argument("--count", type=int, default=1)
    play_sound.add_argument("--interval", type=float, default=0)
    play_sound.set_defaults(func=cmd_play_sound)

    status = sub.add_parser("status", help="read board audio/camera test status")
    add_common_args(status)
    status.set_defaults(func=cmd_status)

    set_volume = sub.add_parser("set-volume", help="set speaker volume")
    add_common_args(set_volume)
    set_volume.add_argument("--volume", type=int, default=100)
    set_volume.set_defaults(func=cmd_set_volume)

    tone = sub.add_parser("tone", help="play a direct PCM sine wave over I2S")
    add_common_args(tone)
    tone.add_argument("--frequency", type=int, default=1000)
    tone.add_argument("--duration-ms", type=int, default=1000)
    tone.add_argument("--amplitude", type=int, default=12000)
    tone.set_defaults(func=cmd_tone)

    play_ogg = sub.add_parser("play-ogg", help="send and play a local Ogg Opus file over USB serial")
    add_common_args(play_ogg)
    play_ogg.add_argument("--file", required=True)
    play_ogg.set_defaults(func=cmd_play_ogg)

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
