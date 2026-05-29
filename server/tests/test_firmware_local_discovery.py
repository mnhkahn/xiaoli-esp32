import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
FIRMWARE_ROOT = REPO_ROOT / "xiaozhi-esp32"


class FirmwareLocalDiscoveryTest(unittest.TestCase):
    def test_ota_tries_local_hwtest_discovery_before_remote_url(self):
        source = (FIRMWARE_ROOT / "main/ota.cc").read_text(encoding="utf-8")

        self.assertIn("DiscoverLocalOtaUrl", source)
        self.assertIn("xiaoli-hwtest-discover-v1", source)
        self.assertIn("CONFIG_XIAOLI_LOCAL_DISCOVERY_PORT", source)
        self.assertIn("CheckVersionFromUrl(local_url)", source)
        self.assertIn("falling back to default OTA URL", source)

    def test_kconfig_exposes_local_discovery_switch(self):
        source = (FIRMWARE_ROOT / "main/Kconfig.projbuild").read_text(encoding="utf-8")

        self.assertIn("config XIAOLI_LOCAL_DISCOVERY", source)
        self.assertIn("config XIAOLI_LOCAL_DISCOVERY_PORT", source)
        self.assertIn("config XIAOLI_LOCAL_DISCOVERY_TIMEOUT_MS", source)


if __name__ == "__main__":
    unittest.main()
