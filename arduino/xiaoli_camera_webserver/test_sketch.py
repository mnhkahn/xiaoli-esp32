#!/usr/bin/env python3
from pathlib import Path


ROOT = Path(__file__).resolve().parent
SKETCH = ROOT / "xiaoli_camera_webserver.ino"


def require(text: str, needle: str) -> None:
    if needle not in text:
        raise AssertionError(f"missing {needle!r}")


def main() -> int:
    text = SKETCH.read_text()

    for needle in [
        "WiFi.softAP",
        "\"/stream\"",
        "\"/capture\"",
        "\"/control\"",
        "FRAMESIZE_QVGA",
        "FRAMESIZE_VGA",
        "FRAMESIZE_SVGA",
        "set_quality",
        "set_sharpness",
        "CAMERA_PIN_D0 11",
        "CAMERA_PIN_XCLK 15",
        "CAMERA_PIN_SIOD 4",
        "CAMERA_PIN_SIOC 5",
    ]:
        require(text, needle)

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
