#!/usr/bin/env python3
from pathlib import Path


ROOT = Path(__file__).resolve().parent
SKETCH = ROOT / "xiaoli_i2s_speaker_test.ino"


def require(text: str, needle: str) -> None:
    if needle not in text:
        raise AssertionError(f"missing {needle!r}")


def main() -> int:
    text = SKETCH.read_text()

    for needle in [
        "#include <ESP_I2S.h>",
        "I2SClass i2s;",
        "GPIO_NUM_39",
        "GPIO_NUM_40",
        "GPIO_NUM_41",
        "sampleRate = 8000",
        "i2s.setPins(I2S_BCLK, I2S_LRC, I2S_DIN)",
        "i2s.begin(mode, sampleRate, bps, slot)",
        "I2S_SLOT_MODE_STEREO",
        "i2s.write(sample)",
    ]:
        require(text, needle)

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
