# Xiaoli Local Hardware Test

This workflow lets the ESP32 prefer a Mac-hosted test server at boot, while
falling back to the normal remote OTA/WebSocket service when the Mac service is
not running.

## Firmware Behavior

On boot, the firmware sends one UDP discovery packet:

```text
xiaoli-hwtest-discover-v1
```

Default destination:

```text
255.255.255.255:8989
```

If a local test service replies with:

```json
{"service":"xiaoli-hwtest","version":1,"ota_url":"http://<mac-ip>:8080/xiaozhi/ota/"}
```

the device calls that OTA URL first. If discovery times out or the local OTA
request fails, the device falls back to the configured `CONFIG_OTA_URL`, for
example `https://xiaoli-server.fly.dev/xiaozhi/ota/`.

## Start Local Service

From the repo root, build the local server image once:

```bash
docker build -t xiaoli-server:hwtest server
```

Start the local server container and UDP discovery responder:

```bash
python3 server/tools/xiaoli_hwtest.py serve \
  --host "$(ipconfig getifaddr en0)" \
  --device-id "28:84:85:8c:ef:f4"
```

Then reboot the ESP32. If discovery succeeds, it will connect to the Mac-hosted
server. If this command is not running, the device continues to use the remote
server.

## Run Hardware Checks

List connected devices:

```bash
python3 server/tools/xiaoli_hwtest.py devices
```

Take a snapshot and save the JPEG:

```bash
python3 server/tools/xiaoli_hwtest.py snapshot \
  --device-id "28:84:85:8c:ef:f4" \
  --resolution vga \
  --out artifacts/hwtest/snapshot.jpg
```

Play a built-in prompt sound:

```bash
python3 server/tools/xiaoli_hwtest.py play-sound \
  --device-id "28:84:85:8c:ef:f4" \
  --name success
```

Queue server-side TTS text:

```bash
python3 server/tools/xiaoli_hwtest.py speak \
  --device-id "28:84:85:8c:ef:f4" \
  --text "测试声音"
```

Play an Ogg Opus file by URL:

```bash
python3 server/tools/xiaoli_hwtest.py play-ogg-url \
  --device-id "28:84:85:8c:ef:f4" \
  --url "http://<mac-ip>:9000/test.ogg"
```

Play a local Ogg Opus file. The helper temporarily serves the file from the Mac
and passes the generated HTTP URL to the board:

```bash
python3 server/tools/xiaoli_hwtest.py play-file \
  --device-id "28:84:85:8c:ef:f4" \
  --file ./test.ogg
```

The ESP32-side playback tool expects Ogg Opus bytes. For MP3/WAV test material,
convert it before playback, for example:

```bash
ffmpeg -i input.wav -c:a libopus test.ogg
```

## Flashing Notes

This change does not modify `main/assets`, fonts, prompt sounds, language
files, or the partition table. For this update, the assets partition does not
need to be refreshed.

Use app-only flashing when the board already has the correct partition table
and assets partition:

```bash
cd xiaozhi-esp32
idf.py build app-flash monitor
```

Use full `idf.py flash` only when you intentionally need to update bootloader,
partition table, or assets. If `CONFIG_FLASH_DEFAULT_ASSETS=y`, full flash may
also rebuild and write `assets.bin`, which is unnecessary for this local
discovery change.
