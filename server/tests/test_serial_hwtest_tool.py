import base64
import importlib.util
import json
import unittest
from unittest import mock
from pathlib import Path


TOOL_PATH = Path(__file__).resolve().parents[1] / "tools" / "xiaoli_serial_hwtest.py"


def load_tool():
    spec = importlib.util.spec_from_file_location("xiaoli_serial_hwtest_under_test", TOOL_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class SerialHwtestToolTest(unittest.TestCase):
    def test_default_baud_matches_stable_firmware_console(self):
        tool = load_tool()

        self.assertEqual(tool.DEFAULT_BAUD, 115200)

    def test_snapshot_command_includes_requested_resolution(self):
        tool = load_tool()

        line = tool.command_line(
            "req-1",
            "snapshot",
            resolution="uxga",
            quality=4,
            settle_ms=800,
            discard_frames=5,
        )

        self.assertTrue(line.startswith(b"XIAOLI_TEST "))
        payload = json.loads(line[len(b"XIAOLI_TEST ") :].decode("utf-8"))
        self.assertEqual(payload["id"], "req-1")
        self.assertEqual(payload["cmd"], "snapshot")
        self.assertEqual(payload["resolution"], "uxga")
        self.assertEqual(payload["quality"], 4)
        self.assertEqual(payload["settle_ms"], 800)
        self.assertEqual(payload["discard_frames"], 5)

    def test_tone_command_includes_direct_pcm_parameters(self):
        tool = load_tool()

        line = tool.command_line("req-1", "tone", frequency=1000, duration_ms=1500, amplitude=12000)

        payload = json.loads(line[len(b"XIAOLI_TEST ") :].decode("utf-8"))
        self.assertEqual(payload["cmd"], "tone")
        self.assertEqual(payload["frequency"], 1000)
        self.assertEqual(payload["duration_ms"], 1500)
        self.assertEqual(payload["amplitude"], 12000)

    def test_status_and_set_volume_commands_are_available(self):
        tool = load_tool()

        status_line = tool.command_line("req-1", "status")
        set_volume_line = tool.command_line("req-2", "set_volume", volume=100)

        status_payload = json.loads(status_line[len(b"XIAOLI_TEST ") :].decode("utf-8"))
        set_volume_payload = json.loads(set_volume_line[len(b"XIAOLI_TEST ") :].decode("utf-8"))
        self.assertEqual(status_payload["cmd"], "status")
        self.assertEqual(set_volume_payload["cmd"], "set_volume")
        self.assertEqual(set_volume_payload["volume"], 100)

    def test_collect_snapshot_decodes_base64_chunks(self):
        tool = load_tool()
        image = b"\xff\xd8test-jpeg\xff\xd9"
        encoded = base64.b64encode(image).decode("ascii")
        lines = [
            'I (123) unrelated log line',
            'XIAOLI_TEST_RESULT {"id":"req-1","ok":true,"content_type":"image/jpeg","length":13,"encoding":"base64"}',
            f'XIAOLI_TEST_DATA {{"id":"req-1","seq":0,"data":"{encoded[:8]}"}}',
            f'XIAOLI_TEST_DATA {{"id":"req-1","seq":1,"data":"{encoded[8:]}"}}',
            'XIAOLI_TEST_END {"id":"req-1","ok":true}',
        ]

        result = tool.collect_snapshot("req-1", lines)

        self.assertEqual(result, image)

    def test_parse_protocol_payload_embedded_in_log_line(self):
        tool = load_tool()

        parsed = tool.parse_prefixed_line(
            'I (729) cam_hal: buffer ready XIAOLI_TEST_RESULT {"id":"req-1","ok":true} extra'
        )

        self.assertEqual(parsed, ("result", {"id": "req-1", "ok": True}))

    def test_parse_incomplete_protocol_payload_is_ignored(self):
        tool = load_tool()

        parsed = tool.parse_prefixed_line('XIAOLI_TEST_DATA {"id":"req-1","data":"abc')

        self.assertIsNone(parsed)

    def test_snapshot_accept_ack_prevents_slow_capture_retry(self):
        tool = load_tool()
        image = b"\xff\xd8slow-jpeg\xff\xd9"
        encoded = base64.b64encode(image).decode("ascii")

        class FakeSerial:
            def __init__(self):
                self.lines = [
                    'XIAOLI_TEST_RESULT {"id":"req-1","ok":true,"cmd":"snapshot","stage":"accepted"}',
                    'XIAOLI_TEST_RESULT {"id":"req-1","ok":true,"cmd":"snapshot","stage":"captured","length":13,"encoding":"base64"}',
                    f'XIAOLI_TEST_DATA {{"id":"req-1","seq":0,"data":"{encoded}"}}',
                    'XIAOLI_TEST_END {"id":"req-1","ok":true}',
                ]

            def read_line(self, timeout):
                return self.lines.pop(0) if self.lines else None

        client = tool.HwtestClient(FakeSerial())

        header, data = client.collect_snapshot_response("req-1", timeout=10, command_timeout=0.01)

        self.assertEqual(header["stage"], "captured")
        self.assertEqual(data, image)

    def test_snapshot_cli_accepts_camera_tuning_options(self):
        tool = load_tool()

        args = tool.build_parser().parse_args([
            "snapshot",
            "--out",
            "/tmp/out.jpg",
            "--quality",
            "4",
            "--settle-ms",
            "800",
            "--discard-frames",
            "5",
        ])

        self.assertEqual(args.quality, 4)
        self.assertEqual(args.settle_ms, 800)
        self.assertEqual(args.discard_frames, 5)

    def test_tone_cli_accepts_frequency_duration_and_amplitude(self):
        tool = load_tool()

        args = tool.build_parser().parse_args([
            "tone",
            "--frequency",
            "1000",
            "--duration-ms",
            "1500",
            "--amplitude",
            "12000",
        ])

        self.assertEqual(args.frequency, 1000)
        self.assertEqual(args.duration_ms, 1500)
        self.assertEqual(args.amplitude, 12000)

    def test_status_and_set_volume_cli_are_available(self):
        tool = load_tool()

        status_args = tool.build_parser().parse_args(["status"])
        set_volume_args = tool.build_parser().parse_args(["set-volume", "--volume", "100"])

        self.assertTrue(callable(status_args.func))
        self.assertEqual(set_volume_args.volume, 100)

    def test_wait_ready_retries_until_ping_response(self):
        tool = load_tool()

        class FakeSerial:
            def __init__(self):
                self.writes = []
                self.clears = 0
                self.lines = [
                    "ESP-ROM:esp32s3-20210327",
                    'XIAOLI_TEST_RESULT {"id":"wrong","ok":true,"cmd":"ping"}',
                    None,
                    'XIAOLI_TEST_RESULT {"id":"ready-2","ok":true,"cmd":"ping"}',
                ]

            def write_line(self, line):
                self.writes.append(line)

            def clear_input_line(self):
                self.clears += 1

            def read_line(self, timeout):
                item = self.lines.pop(0)
                return item

        fake = FakeSerial()
        client = tool.HwtestClient(fake)
        ids = iter(["ready-1", "ready-2"])
        client.request_id = lambda: next(ids)

        result = client.wait_ready(timeout=5, probe_timeout=0.01)

        self.assertEqual(result["id"], "ready-2")
        self.assertEqual(len(fake.writes), 2)
        self.assertEqual(fake.clears, 2)

    def test_batch_snapshot_cli_accepts_resolutions_and_out_dir(self):
        tool = load_tool()

        args = tool.build_parser().parse_args([
            "batch-snapshot",
            "--out-dir",
            "/tmp/camera",
            "--resolutions",
            "qvga",
            "vga",
            "uxga",
        ])

        self.assertEqual(args.out_dir, "/tmp/camera")
        self.assertEqual(args.resolutions, ["qvga", "vga", "uxga"])

    def test_timelapse_snapshot_cli_accepts_interval_count_and_out_dir(self):
        tool = load_tool()

        args = tool.build_parser().parse_args([
            "timelapse-snapshot",
            "--out-dir",
            "/tmp/camera",
            "--resolution",
            "svga",
            "--interval",
            "5",
            "--count",
            "12",
        ])

        self.assertEqual(args.out_dir, "/tmp/camera")
        self.assertEqual(args.resolution, "svga")
        self.assertEqual(args.interval, 5)
        self.assertEqual(args.count, 12)

    def test_serial_write_line_retries_partial_nonblocking_writes(self):
        tool = load_tool()
        serial = tool.SerialPort.__new__(tool.SerialPort)
        serial.fd = 123
        written_chunks = []

        def fake_write(fd, data):
            chunk = bytes(data[:3])
            written_chunks.append(chunk)
            return len(chunk)

        with mock.patch.object(tool.os, "write", side_effect=fake_write), \
             mock.patch.object(tool.termios, "tcdrain") as tcdrain:
            serial.write_line(b"XIAOLI_TEST {}\n")

        self.assertEqual(b"".join(written_chunks), b"XIAOLI_TEST {}\n")
        tcdrain.assert_called_once_with(123)

    def test_client_send_clears_pending_line_before_command(self):
        tool = load_tool()

        class FakeSerial:
            def __init__(self):
                self.calls = []

            def clear_input_line(self):
                self.calls.append("clear")

            def write_line(self, line):
                self.calls.append(("write", line))

        fake = FakeSerial()
        client = tool.HwtestClient(fake)

        client.send("req-1", "ping")

        self.assertEqual(fake.calls[0], "clear")
        self.assertTrue(fake.calls[1][1].startswith(b"XIAOLI_TEST "))

    def test_snapshot_retries_when_header_never_arrives(self):
        tool = load_tool()

        class FakeSerial:
            def __init__(self):
                self.writes = []
                self.clears = 0
                self.lines = [
                    None,
                    'XIAOLI_TEST_RESULT {"id":"try-2","ok":true,"cmd":"snapshot","encoding":"base64"}',
                    'XIAOLI_TEST_END {"id":"try-2","ok":true}',
                ]

            def clear_input_line(self):
                self.clears += 1

            def write_line(self, line):
                self.writes.append(line)

            def read_line(self, timeout):
                return self.lines.pop(0)

        fake = FakeSerial()
        client = tool.HwtestClient(fake)
        ids = iter(["try-1", "try-2"])
        client.request_id = lambda: next(ids)

        header, image = client.snapshot("qvga", timeout=0.01, attempts=2, command_timeout=0.01)

        self.assertEqual(header["id"], "try-2")
        self.assertEqual(image, b"")
        self.assertEqual(len(fake.writes), 2)


if __name__ == "__main__":
    unittest.main()
