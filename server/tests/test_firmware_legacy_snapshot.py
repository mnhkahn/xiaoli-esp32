import re
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
FIRMWARE_ROOT = REPO_ROOT / "xiaozhi-esp32"


class FirmwareLegacySnapshotTest(unittest.TestCase):
    def test_snapshot_tool_advertises_legacy_vga(self):
        source = (FIRMWARE_ROOT / "main/mcp_server.cc").read_text(encoding="utf-8")

        self.assertIn("legacy_vga", source)
        self.assertIn("qvga, vga, svga, xga, uxga, or legacy_vga", source)

    def test_legacy_vga_uses_original_capture_profile(self):
        source = (FIRMWARE_ROOT / "main/boards/common/esp32_camera.cc").read_text(encoding="utf-8")

        self.assertRegex(
            source,
            re.compile(
                r'\{\s*"legacy_vga"\s*,\s*FRAMESIZE_VGA\s*,\s*12\s*,'
                r"\s*PIXFORMAT_RGB565\s*,\s*80\s*,\s*true\s*\}"
            ),
        )


if __name__ == "__main__":
    unittest.main()
