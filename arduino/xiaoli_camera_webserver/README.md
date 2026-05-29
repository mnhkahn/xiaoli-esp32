# Xiaoli Camera WebServer

Standalone Arduino firmware for checking the Xiaoli ESP32-S3 camera in a browser.

This sketch is separate from the normal `xiaozhi-esp32` firmware. Uploading it
will replace the currently running Xiaoli firmware until you flash `xiaozhi-esp32`
again.

## Behavior

- Starts a local Wi-Fi access point by default.
- AP SSID: `Xiaoli-Camera`
- AP password: `xiaoli123456`
- Web UI: `http://192.168.4.1/`
- MJPEG stream: `http://192.168.4.1:81/stream`
- Single capture: `http://192.168.4.1/capture`

The web UI can switch resolution and JPEG quality at runtime.

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

Open `xiaoli_camera_webserver.ino` in Arduino IDE, then upload.

## arduino-cli

Compile:

```sh
arduino-cli compile \
  --fqbn "esp32:esp32:esp32s3:FlashSize=16M,PSRAM=opi,PartitionScheme=huge_app,USBMode=hwcdc,CDCOnBoot=cdc,UploadMode=default,UploadSpeed=921600" \
  arduino/xiaoli_camera_webserver
```

Upload:

```sh
arduino-cli upload \
  -p /dev/cu.usbserial-14310 \
  --fqbn "esp32:esp32:esp32s3:FlashSize=16M,PSRAM=opi,PartitionScheme=huge_app,USBMode=hwcdc,CDCOnBoot=cdc,UploadMode=default,UploadSpeed=921600" \
  arduino/xiaoli_camera_webserver
```

## Router Wi-Fi mode

By default the sketch uses AP mode. To connect it to a router instead, edit:

```cpp
static const char *WIFI_SSID = "";
static const char *WIFI_PASSWORD = "";
```

If router Wi-Fi connection fails, it falls back to AP mode.
