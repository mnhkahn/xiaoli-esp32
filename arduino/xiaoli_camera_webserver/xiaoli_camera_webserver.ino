#include <Arduino.h>
#include <WiFi.h>
#include "esp_camera.h"
#include "esp_http_server.h"
#include "esp_timer.h"

// Xiaoli Bread Compact WiFi S3 Cam camera pins.
#define CAMERA_PIN_D0 11
#define CAMERA_PIN_D1 9
#define CAMERA_PIN_D2 8
#define CAMERA_PIN_D3 10
#define CAMERA_PIN_D4 12
#define CAMERA_PIN_D5 18
#define CAMERA_PIN_D6 17
#define CAMERA_PIN_D7 16
#define CAMERA_PIN_XCLK 15
#define CAMERA_PIN_PCLK 13
#define CAMERA_PIN_VSYNC 6
#define CAMERA_PIN_HREF 7
#define CAMERA_PIN_SIOD 4
#define CAMERA_PIN_SIOC 5
#define CAMERA_PIN_PWDN -1
#define CAMERA_PIN_RESET -1
#define CAMERA_XCLK_HZ 20000000

// Leave WIFI_SSID empty to start a local access point.
static const char *WIFI_SSID = "";
static const char *WIFI_PASSWORD = "";
static const char *AP_SSID = "Xiaoli-Camera";
static const char *AP_PASSWORD = "xiaoli123456";

static httpd_handle_t main_httpd = NULL;
static httpd_handle_t stream_httpd = NULL;

static const char *current_resolution = "vga";
static int current_quality = 12;
static int current_sharpness = 2;
static int current_contrast = 1;
static int current_brightness = 0;
static int current_saturation = -1;
static int current_denoise = 0;

struct FrameSizeOption {
  const char *name;
  framesize_t frame_size;
};

static const FrameSizeOption FRAME_SIZES[] = {
    {"qqvga", FRAMESIZE_QQVGA},
    {"qvga", FRAMESIZE_QVGA},
    {"vga", FRAMESIZE_VGA},
    {"svga", FRAMESIZE_SVGA},
    {"xga", FRAMESIZE_XGA},
    {"sxga", FRAMESIZE_SXGA},
    {"uxga", FRAMESIZE_UXGA},
};

#define PART_BOUNDARY "xiaoli_frame_boundary"
static const char *STREAM_CONTENT_TYPE = "multipart/x-mixed-replace;boundary=" PART_BOUNDARY;
static const char *STREAM_BOUNDARY = "\r\n--" PART_BOUNDARY "\r\n";
static const char *STREAM_PART = "Content-Type: image/jpeg\r\nContent-Length: %u\r\n\r\n";

static const char INDEX_HTML[] PROGMEM = R"HTML(
<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Xiaoli Camera</title>
  <style>
    body { margin: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: #111; color: #eee; }
    header { display: flex; gap: 12px; align-items: center; flex-wrap: wrap; padding: 10px 12px; background: #1f1f1f; }
    button, select, input { height: 32px; border: 1px solid #555; background: #2a2a2a; color: #eee; border-radius: 4px; padding: 0 10px; }
    label { display: inline-flex; align-items: center; gap: 6px; font-size: 14px; }
    main { display: grid; place-items: center; min-height: calc(100vh - 54px); }
    img { max-width: 100vw; max-height: calc(100vh - 54px); width: auto; height: auto; background: #000; }
    .status { color: #aaa; font-size: 13px; }
  </style>
</head>
<body>
  <header>
    <label>Resolution
      <select id="res" onchange="setCamera()">
        <option value="qqvga">QQVGA</option>
        <option value="qvga">QVGA</option>
        <option value="vga" selected>VGA</option>
        <option value="svga">SVGA</option>
        <option value="xga">XGA</option>
        <option value="sxga">SXGA</option>
        <option value="uxga">UXGA</option>
      </select>
    </label>
    <label>JPEG
      <input id="quality" type="number" min="4" max="40" value="12" onchange="setCamera()">
    </label>
    <label>Sharpness
      <input id="sharpness" type="number" min="-2" max="2" value="2" onchange="setCamera()">
    </label>
    <button onclick="snapshot()">Capture</button>
    <span id="status" class="status"></span>
  </header>
  <main>
    <img id="stream" alt="camera stream">
  </main>
  <script>
    const stream = document.getElementById('stream');
    const statusEl = document.getElementById('status');

    function streamUrl() {
      return `http://${location.hostname}:81/stream?ts=${Date.now()}`;
    }

    function restartStream() {
      stream.src = '';
      setTimeout(() => stream.src = streamUrl(), 150);
    }

    async function setCamera() {
      const params = new URLSearchParams({
        res: document.getElementById('res').value,
        quality: document.getElementById('quality').value,
        sharpness: document.getElementById('sharpness').value
      });
      statusEl.textContent = 'Applying...';
      const resp = await fetch(`/control?${params.toString()}`);
      statusEl.textContent = await resp.text();
      restartStream();
    }

    function snapshot() {
      window.open('/capture?ts=' + Date.now(), '_blank');
    }

    restartStream();
  </script>
</body>
</html>
)HTML";

static const FrameSizeOption *find_frame_size(const char *name) {
  for (size_t i = 0; i < sizeof(FRAME_SIZES) / sizeof(FRAME_SIZES[0]); ++i) {
    if (strcmp(name, FRAME_SIZES[i].name) == 0) {
      return &FRAME_SIZES[i];
    }
  }
  return NULL;
}

static int clamp_int(int value, int min_value, int max_value) {
  if (value < min_value) {
    return min_value;
  }
  if (value > max_value) {
    return max_value;
  }
  return value;
}

static bool get_query_value(httpd_req_t *req, const char *key, char *out, size_t out_len) {
  size_t query_len = httpd_req_get_url_query_len(req) + 1;
  if (query_len <= 1) {
    return false;
  }

  char *query = static_cast<char *>(malloc(query_len));
  if (!query) {
    return false;
  }

  bool found = false;
  if (httpd_req_get_url_query_str(req, query, query_len) == ESP_OK &&
      httpd_query_key_value(query, key, out, out_len) == ESP_OK) {
    found = true;
  }

  free(query);
  return found;
}

static int get_query_int(httpd_req_t *req, const char *key, int fallback, int min_value, int max_value) {
  char value[16] = {};
  if (!get_query_value(req, key, value, sizeof(value))) {
    return fallback;
  }
  return clamp_int(atoi(value), min_value, max_value);
}

static void apply_sensor_settings() {
  sensor_t *sensor = esp_camera_sensor_get();
  if (!sensor) {
    return;
  }

  sensor->set_quality(sensor, current_quality);
  sensor->set_sharpness(sensor, current_sharpness);
  sensor->set_contrast(sensor, current_contrast);
  sensor->set_brightness(sensor, current_brightness);
  sensor->set_saturation(sensor, current_saturation);
  sensor->set_denoise(sensor, current_denoise);
  sensor->set_lenc(sensor, 1);
  sensor->set_bpc(sensor, 1);
  sensor->set_wpc(sensor, 1);
  sensor->set_hmirror(sensor, 0);
  sensor->set_vflip(sensor, 0);

  const FrameSizeOption *frame_size = find_frame_size(current_resolution);
  if (frame_size) {
    sensor->set_framesize(sensor, frame_size->frame_size);
  }
}

static esp_err_t index_handler(httpd_req_t *req) {
  httpd_resp_set_type(req, "text/html");
  return httpd_resp_send(req, INDEX_HTML, HTTPD_RESP_USE_STRLEN);
}

static esp_err_t control_handler(httpd_req_t *req) {
  char resolution[16] = {};
  if (get_query_value(req, "res", resolution, sizeof(resolution))) {
    const FrameSizeOption *frame_size = find_frame_size(resolution);
    if (!frame_size) {
      httpd_resp_set_status(req, "400 Bad Request");
      return httpd_resp_sendstr(req, "unknown resolution");
    }
    current_resolution = frame_size->name;
  }

  current_quality = get_query_int(req, "quality", current_quality, 4, 40);
  current_sharpness = get_query_int(req, "sharpness", current_sharpness, -2, 2);
  current_contrast = get_query_int(req, "contrast", current_contrast, -2, 2);
  current_brightness = get_query_int(req, "brightness", current_brightness, -2, 2);
  current_saturation = get_query_int(req, "saturation", current_saturation, -2, 2);
  current_denoise = get_query_int(req, "denoise", current_denoise, 0, 1);

  apply_sensor_settings();

  char response[160] = {};
  snprintf(response, sizeof(response),
           "res=%s quality=%d sharpness=%d contrast=%d brightness=%d saturation=%d denoise=%d",
           current_resolution, current_quality, current_sharpness, current_contrast,
           current_brightness, current_saturation, current_denoise);
  httpd_resp_set_type(req, "text/plain");
  httpd_resp_set_hdr(req, "Access-Control-Allow-Origin", "*");
  return httpd_resp_sendstr(req, response);
}

static esp_err_t capture_handler(httpd_req_t *req) {
  camera_fb_t *fb = esp_camera_fb_get();
  if (!fb) {
    httpd_resp_send_500(req);
    return ESP_FAIL;
  }

  esp_err_t result = ESP_OK;
  if (fb->format != PIXFORMAT_JPEG) {
    httpd_resp_send_500(req);
    result = ESP_FAIL;
  } else {
    httpd_resp_set_type(req, "image/jpeg");
    httpd_resp_set_hdr(req, "Content-Disposition", "inline; filename=xiaoli.jpg");
    httpd_resp_set_hdr(req, "Access-Control-Allow-Origin", "*");
    result = httpd_resp_send(req, reinterpret_cast<const char *>(fb->buf), fb->len);
  }

  esp_camera_fb_return(fb);
  return result;
}

static esp_err_t stream_handler(httpd_req_t *req) {
  esp_err_t result = httpd_resp_set_type(req, STREAM_CONTENT_TYPE);
  if (result != ESP_OK) {
    return result;
  }
  httpd_resp_set_hdr(req, "Access-Control-Allow-Origin", "*");

  char part_buf[96] = {};
  while (true) {
    camera_fb_t *fb = esp_camera_fb_get();
    if (!fb) {
      delay(50);
      continue;
    }

    size_t part_len = snprintf(part_buf, sizeof(part_buf), STREAM_PART, static_cast<unsigned int>(fb->len));
    result = httpd_resp_send_chunk(req, STREAM_BOUNDARY, strlen(STREAM_BOUNDARY));
    if (result == ESP_OK) {
      result = httpd_resp_send_chunk(req, part_buf, part_len);
    }
    if (result == ESP_OK) {
      result = httpd_resp_send_chunk(req, reinterpret_cast<const char *>(fb->buf), fb->len);
    }

    esp_camera_fb_return(fb);
    if (result != ESP_OK) {
      break;
    }

    delay(1);
  }

  return result;
}

static void register_uri(httpd_handle_t server, const char *uri, esp_err_t (*handler)(httpd_req_t *)) {
  httpd_uri_t item = {};
  item.uri = uri;
  item.method = HTTP_GET;
  item.handler = handler;
  item.user_ctx = NULL;
  httpd_register_uri_handler(server, &item);
}

static void start_camera_server() {
  httpd_config_t main_config = HTTPD_DEFAULT_CONFIG();
  main_config.server_port = 80;
  main_config.stack_size = 8192;
  main_config.max_uri_handlers = 8;

  if (httpd_start(&main_httpd, &main_config) == ESP_OK) {
    register_uri(main_httpd, "/", index_handler);
    register_uri(main_httpd, "/capture", capture_handler);
    register_uri(main_httpd, "/control", control_handler);
  }

  httpd_config_t stream_config = HTTPD_DEFAULT_CONFIG();
  stream_config.server_port = 81;
  stream_config.ctrl_port = main_config.ctrl_port + 1;
  stream_config.stack_size = 8192;
  stream_config.max_uri_handlers = 4;

  if (httpd_start(&stream_httpd, &stream_config) == ESP_OK) {
    register_uri(stream_httpd, "/stream", stream_handler);
  }
}

static bool start_wifi_sta() {
  if (strlen(WIFI_SSID) == 0) {
    return false;
  }

  WiFi.mode(WIFI_STA);
  WiFi.setSleep(false);
  WiFi.begin(WIFI_SSID, WIFI_PASSWORD);

  Serial.printf("Connecting to WiFi SSID: %s", WIFI_SSID);
  for (int i = 0; i < 30; ++i) {
    if (WiFi.status() == WL_CONNECTED) {
      Serial.println();
      Serial.printf("STA ready: http://%s/\n", WiFi.localIP().toString().c_str());
      Serial.printf("Stream: http://%s:81/stream\n", WiFi.localIP().toString().c_str());
      return true;
    }
    Serial.print(".");
    delay(500);
  }

  Serial.println();
  return false;
}

static void start_wifi_ap() {
  WiFi.mode(WIFI_AP);
  WiFi.setSleep(false);
  WiFi.softAP(AP_SSID, AP_PASSWORD);
  IPAddress ip = WiFi.softAPIP();
  Serial.printf("AP ready: SSID=%s password=%s\n", AP_SSID, AP_PASSWORD);
  Serial.printf("Open: http://%s/\n", ip.toString().c_str());
  Serial.printf("Stream: http://%s:81/stream\n", ip.toString().c_str());
}

static bool init_camera() {
  camera_config_t config = {};
  config.ledc_channel = LEDC_CHANNEL_0;
  config.ledc_timer = LEDC_TIMER_0;
  config.pin_d0 = CAMERA_PIN_D0;
  config.pin_d1 = CAMERA_PIN_D1;
  config.pin_d2 = CAMERA_PIN_D2;
  config.pin_d3 = CAMERA_PIN_D3;
  config.pin_d4 = CAMERA_PIN_D4;
  config.pin_d5 = CAMERA_PIN_D5;
  config.pin_d6 = CAMERA_PIN_D6;
  config.pin_d7 = CAMERA_PIN_D7;
  config.pin_xclk = CAMERA_PIN_XCLK;
  config.pin_pclk = CAMERA_PIN_PCLK;
  config.pin_vsync = CAMERA_PIN_VSYNC;
  config.pin_href = CAMERA_PIN_HREF;
  config.pin_sccb_sda = CAMERA_PIN_SIOD;
  config.pin_sccb_scl = CAMERA_PIN_SIOC;
  config.pin_pwdn = CAMERA_PIN_PWDN;
  config.pin_reset = CAMERA_PIN_RESET;
  config.xclk_freq_hz = CAMERA_XCLK_HZ;
  config.pixel_format = PIXFORMAT_JPEG;
  config.frame_size = FRAMESIZE_VGA;
  config.jpeg_quality = current_quality;
  config.grab_mode = CAMERA_GRAB_LATEST;

  if (psramFound()) {
    config.fb_location = CAMERA_FB_IN_PSRAM;
    config.fb_count = 2;
  } else {
    config.fb_location = CAMERA_FB_IN_DRAM;
    config.frame_size = FRAMESIZE_QVGA;
    config.fb_count = 1;
    current_resolution = "qvga";
  }

  esp_err_t error = esp_camera_init(&config);
  if (error != ESP_OK) {
    Serial.printf("Camera init failed: 0x%x\n", error);
    return false;
  }

  sensor_t *sensor = esp_camera_sensor_get();
  if (sensor) {
    Serial.printf("Camera PID: 0x%04x\n", sensor->id.PID);
  }

  apply_sensor_settings();
  return true;
}

void setup() {
  Serial.begin(115200);
  Serial.setDebugOutput(true);
  delay(1000);

  Serial.println();
  Serial.println("Xiaoli Camera WebServer");

  if (!init_camera()) {
    return;
  }

  if (!start_wifi_sta()) {
    start_wifi_ap();
  }

  start_camera_server();
  Serial.println("Camera web server started");
}

void loop() {
  delay(10000);
}
