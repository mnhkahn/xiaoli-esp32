import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
CAMERA_SOURCE = ROOT / "xiaozhi-esp32" / "main" / "boards" / "common" / "esp32_camera.cc"


class FirmwareCameraStreamTest(unittest.TestCase):
    def test_camera_stream_resolution_is_runtime_configurable(self):
        source = CAMERA_SOURCE.read_text(encoding="utf-8")
        camera_header = (ROOT / "xiaozhi-esp32" / "main" / "boards" / "common" / "camera.h").read_text(encoding="utf-8")
        camera_impl_header = (ROOT / "xiaozhi-esp32" / "main" / "boards" / "common" / "esp32_camera.h").read_text(encoding="utf-8")
        mcp_source = (ROOT / "xiaozhi-esp32" / "main" / "mcp_server.cc").read_text(encoding="utf-8")

        self.assertIn("StartStream(int fps, int duration_sec, const std::string& resolution)", camera_header)
        self.assertIn("StartStream(int fps, int duration_sec, const std::string& resolution, const std::string& transport) override", camera_impl_header)
        self.assertIn('{"qqvga", FRAMESIZE_QQVGA}', source)
        self.assertIn('{"qvga", FRAMESIZE_QVGA}', source)
        self.assertIn('{"vga", FRAMESIZE_VGA}', source)
        self.assertIn("ResolveStreamResolution(resolution)", source)
        self.assertIn("SetSensorProfileLocked(frame_size, kStreamJpegQuality, \"stream\")", source)
        self.assertIn('Property("resolution", kPropertyTypeString, std::string("qqvga"))', mcp_source)
        self.assertIn("camera->StartStream(fps, duration_sec, resolution, transport)", mcp_source)
        self.assertIn('{"qvga", FRAMESIZE_QVGA', source)

    def test_camera_exposes_lan_mjpeg_stream(self):
        source = CAMERA_SOURCE.read_text(encoding="utf-8")
        camera_header = (ROOT / "xiaozhi-esp32" / "main" / "boards" / "common" / "camera.h").read_text(encoding="utf-8")
        camera_impl_header = (ROOT / "xiaozhi-esp32" / "main" / "boards" / "common" / "esp32_camera.h").read_text(encoding="utf-8")
        mcp_source = (ROOT / "xiaozhi-esp32" / "main" / "mcp_server.cc").read_text(encoding="utf-8")
        cmake_source = (ROOT / "xiaozhi-esp32" / "main" / "CMakeLists.txt").read_text(encoding="utf-8")
        wifi_board_source = (ROOT / "xiaozhi-esp32" / "main" / "boards" / "common" / "wifi_board.cc").read_text(encoding="utf-8")

        self.assertIn("StartStream(int fps, int duration_sec, const std::string& resolution, const std::string& transport)", camera_header)
        self.assertIn("StartLanServer()", camera_header)
        self.assertIn("StartLanStreamLocked", camera_impl_header)
        self.assertIn("#include <esp_http_server.h>", source)
        self.assertIn("XIAOLI_LAN_STREAM_PORT", source)
        self.assertIn('"/stream"', source)
        self.assertIn('"/capture"', source)
        self.assertIn("multipart/x-mixed-replace", source)
        self.assertIn("GetLanStreamUrlLocked", source)
        self.assertIn('cJSON_AddStringToObject(root, "transport", "lan")', source)
        self.assertIn('Property("transport", kPropertyTypeString, std::string("auto"))', mcp_source)
        self.assertIn("camera->StartStream(fps, duration_sec, resolution, transport)", mcp_source)
        self.assertIn("esp_http_server", cmake_source)
        self.assertIn("camera->StartLanServer()", wifi_board_source)


if __name__ == "__main__":
    unittest.main()
