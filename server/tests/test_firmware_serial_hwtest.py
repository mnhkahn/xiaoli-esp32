import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
MAIN = ROOT / "xiaozhi-esp32" / "main"


class FirmwareSerialHwtestTest(unittest.TestCase):
    def test_firmware_exposes_serial_hwtest_command_task(self):
        source = (MAIN / "serial_hwtest.cc").read_text(encoding="utf-8")
        app_source = (MAIN / "application.cc").read_text(encoding="utf-8")
        cmake = (MAIN / "CMakeLists.txt").read_text(encoding="utf-8")
        kconfig = (MAIN / "Kconfig.projbuild").read_text(encoding="utf-8")
        sdkconfig = (ROOT / "xiaozhi-esp32" / "sdkconfig").read_text(encoding="utf-8")
        sdkconfig_defaults = (ROOT / "xiaozhi-esp32" / "sdkconfig.defaults").read_text(encoding="utf-8")

        self.assertIn("XIAOLI_TEST ", source)
        self.assertIn('"snapshot"', source)
        self.assertIn('"play_sound"', source)
        self.assertIn('"status"', source)
        self.assertIn('"set_volume"', source)
        self.assertIn("HandleStatus", source)
        self.assertIn("HandleSetVolume", source)
        self.assertIn("codec->output_volume()", source)
        self.assertIn('"tone"', source)
        self.assertIn("HandleTone", source)
        self.assertIn("std::sin", source)
        self.assertIn('"play_ogg_begin"', source)
        self.assertIn("StartSerialHwtest", app_source)
        self.assertIn("serial_hwtest.cc", cmake)
        self.assertIn("config XIAOLI_SERIAL_HWTEST", kconfig)
        self.assertIn("FindCommandJson", source)
        self.assertIn("cJSON_ParseWithOpts", source)
        self.assertIn("read(fileno(stdin)", source)
        self.assertNotIn("fgets(line", source)
        self.assertIn("kSnapshotYieldEveryChunks", source)
        self.assertIn("vTaskDelay(pdMS_TO_TICKS(1))", source)
        self.assertIn("ScopedLogSilencer", source)
        self.assertIn("esp_log_set_vprintf(&SilentLogVprintf)", source)
        self.assertIn("esp_log_set_vprintf(previous_vprintf_)", source)
        self.assertIn("\\\"stage\\\":\\\"accepted\\\"", source)
        self.assertLess(source.index("\\\"stage\\\":\\\"accepted\\\""), source.index("SnapshotToJpeg(resolution"))
        self.assertIn("CONFIG_ESP_CONSOLE_UART_DEFAULT=y", sdkconfig)
        self.assertIn("# CONFIG_ESP_CONSOLE_UART_CUSTOM is not set", sdkconfig)
        self.assertIn("CONFIG_ESP_CONSOLE_UART_BAUDRATE=115200", sdkconfig)
        self.assertIn("CONFIG_ESPTOOLPY_MONITOR_BAUD=115200", sdkconfig)
        self.assertIn("CONFIG_ESPTOOLPY_MONITOR_BAUD=115200", sdkconfig_defaults)

    def test_s3cam_speaker_uses_explicit_i2s_slot_mask(self):
        board_source = (MAIN / "boards" / "bread-compact-wifi-s3cam" / "compact_wifi_board_s3cam.cc").read_text(encoding="utf-8")

        self.assertIn("I2S_STD_SLOT_BOTH", board_source)
        self.assertIn("AUDIO_I2S_SPK_GPIO_DOUT, I2S_STD_SLOT_BOTH", board_source)

    def test_camera_snapshot_to_jpeg_accepts_resolution_without_upload(self):
        camera_header = (MAIN / "boards" / "common" / "camera.h").read_text(encoding="utf-8")
        esp32_camera = (MAIN / "boards" / "common" / "esp32_camera.cc").read_text(encoding="utf-8")
        serial_source = (MAIN / "serial_hwtest.cc").read_text(encoding="utf-8")

        self.assertIn("SnapshotToJpeg", camera_header)
        self.assertIn("CameraSnapshotOptions", camera_header)
        self.assertIn("SnapshotToJpeg(const std::string& resolution", esp32_camera)
        self.assertIn("quality", serial_source)
        self.assertIn("settle_ms", serial_source)
        self.assertIn("discard_frames", serial_source)
        self.assertIn("SnapshotToJpeg(resolution, options", serial_source)
        self.assertNotIn("UploadSnapshotLocked(resolution", serial_source)


if __name__ == "__main__":
    unittest.main()
