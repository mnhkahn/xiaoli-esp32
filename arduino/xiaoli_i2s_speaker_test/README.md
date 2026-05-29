# Xiaoli I2S Speaker Test

Standalone Arduino firmware for isolating Xiaoli ESP32-S3 speaker output.

Uploading this sketch replaces the normal Xiaozhi firmware until you flash
`xiaozhi-esp32` again.

## Behavior

- Uses the same speaker pins as the Xiaozhi board config:
  - BCLK: GPIO 40
  - LRCK/WS: GPIO 41
  - DOUT: GPIO 39
- Plays repeated 2-second tones in this order:
  - left channel only
  - right channel only
  - both channels
- Prints the active channel to USB serial at 115200 baud.

If any segment is audible, the speaker hardware path works and the problem is
inside the Xiaozhi firmware audio path. If no segment is audible, check the
speaker pins, amplifier enable/shutdown wiring, speaker power, and speaker
connection.

## Arduino IDE

Use these settings:

- Board: `ESP32S3 Dev Module`
- USB Mode: `Hardware CDC and JTAG`
- USB CDC On Boot: `Enabled`
- Upload Mode: `UART0 / Hardware CDC`
- Flash Size: `16MB`
- PSRAM: `OPI PSRAM`
- Partition Scheme: `Huge APP (3MB No OTA/1MB SPIFFS)`
- Upload Speed: `921600`

Open `xiaoli_i2s_speaker_test.ino`, then upload.

## arduino-cli

Compile:

```sh
arduino-cli compile \
  --fqbn "esp32:esp32:esp32s3:FlashSize=16M,PSRAM=opi,PartitionScheme=huge_app,USBMode=hwcdc,CDCOnBoot=cdc,UploadMode=default,UploadSpeed=921600" \
  arduino/xiaoli_i2s_speaker_test
```

Upload:

```sh
arduino-cli upload \
  -p /dev/cu.usbserial-14310 \
  --fqbn "esp32:esp32:esp32s3:FlashSize=16M,PSRAM=opi,PartitionScheme=huge_app,USBMode=hwcdc,CDCOnBoot=cdc,UploadMode=default,UploadSpeed=921600" \
  arduino/xiaoli_i2s_speaker_test
```
