#include "sdkconfig.h"

#include <algorithm>
#include <esp_heap_caps.h>
#include <cctype>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <cJSON.h>
#include <esp_log.h>
#include <esp_http_server.h>
#include <cmath>
#include <img_converters.h>
#include <memory>
#include <new>
#include <mbedtls/base64.h>
#include <wifi_manager.h>

#include "esp32_camera.h"
#include "application.h"
#include "board.h"
#include "assets/lang_config.h"
#include "display.h"
#include "lvgl_display.h"
#include "mcp_server.h"
#include "system_info.h"
#include "jpg/image_to_jpeg.h"
#include "esp_timer.h"

#define TAG "Esp32Camera"
#define XIAOLI_LAN_STREAM_PORT 8081

namespace {
constexpr uint32_t kStreamTaskStackSize = 12 * 1024;
constexpr UBaseType_t kStreamTaskPriority = 4;
constexpr framesize_t kDefaultStreamFrameSize = FRAMESIZE_QQVGA;
constexpr int kStreamJpegQuality = 30;
constexpr int kLanStreamJpegQuality = 12;
constexpr framesize_t kPhotoFrameSize = FRAMESIZE_UXGA;
constexpr int kPhotoJpegQuality = 8;
constexpr int kSensorSettleMs = 150;
constexpr TickType_t kSensorSettleDelay = pdMS_TO_TICKS(kSensorSettleMs);
constexpr int kDefaultSnapshotDiscardFrames = 1;
constexpr const char* kLanStreamContentType = "multipart/x-mixed-replace;boundary=xiaoli_lan_frame";
constexpr const char* kLanStreamBoundaryLine = "\r\n--xiaoli_lan_frame\r\n";
constexpr const char* kLanStreamPartHeader = "Content-Type: image/jpeg\r\nContent-Length: %u\r\n\r\n";

struct StreamTaskArgs {
    Esp32Camera* camera;
    std::string stream_id;
    int fps;
    int duration_sec;
    framesize_t frame_size;
    std::string resolution;
};

struct SnapshotResolution {
    const char* name;
    framesize_t frame_size;
    int jpeg_quality;
    pixformat_t pixel_format;
    int encode_quality;
    bool reinitialize;
};

struct StreamResolution {
    const char* name;
    framesize_t frame_size;
};

const SnapshotResolution kSnapshotResolutions[] = {
    {"qqvga", FRAMESIZE_QQVGA, 20, PIXFORMAT_JPEG, 20, false},
    {"qvga", FRAMESIZE_QVGA, 16, PIXFORMAT_JPEG, 16, false},
    {"vga", FRAMESIZE_VGA, 12, PIXFORMAT_JPEG, 12, false},
    {"svga", FRAMESIZE_SVGA, 10, PIXFORMAT_JPEG, 10, false},
    {"xga", FRAMESIZE_XGA, 10, PIXFORMAT_JPEG, 10, false},
    {"uxga", FRAMESIZE_UXGA, 8, PIXFORMAT_JPEG, 8, false},
    {"legacy_vga", FRAMESIZE_VGA, 12, PIXFORMAT_RGB565, 80, true},
};

const StreamResolution kStreamResolutions[] = {
    {"qqvga", FRAMESIZE_QQVGA},
    {"qvga", FRAMESIZE_QVGA},
    {"vga", FRAMESIZE_VGA},
    {"svga", FRAMESIZE_SVGA},
};

std::string NormalizeResolutionName(const std::string& value) {
    std::string normalized = value;
    std::transform(normalized.begin(), normalized.end(), normalized.begin(),
        [](unsigned char c) { return static_cast<char>(std::tolower(c)); });
    return normalized;
}

const SnapshotResolution& ResolveSnapshotResolution(const std::string& value) {
    std::string normalized = NormalizeResolutionName(value);
    for (const auto& resolution : kSnapshotResolutions) {
        if (normalized == resolution.name) {
            return resolution;
        }
    }
    return kSnapshotResolutions[1];
}

const StreamResolution& ResolveStreamResolution(const std::string& value) {
    std::string normalized = NormalizeResolutionName(value);
    for (const auto& resolution : kStreamResolutions) {
        if (normalized == resolution.name) {
            return resolution;
        }
    }
    return kStreamResolutions[0];
}

bool IsLanStreamTransport(const std::string& value) {
    auto normalized = NormalizeResolutionName(value);
    return normalized == "lan" || normalized == "mjpeg" || normalized == "local";
}

bool IsAutoStreamTransport(const std::string& value) {
    return NormalizeResolutionName(value) == "auto";
}

std::string JsonString(cJSON* root) {
    char* json_str = cJSON_PrintUnformatted(root);
    std::string result = json_str ? json_str : "{}";
    if (json_str) {
        cJSON_free(json_str);
    }
    cJSON_Delete(root);
    return result;
}

std::string JsonError(const char* error) {
    cJSON* root = cJSON_CreateObject();
    cJSON_AddBoolToObject(root, "ok", false);
    cJSON_AddStringToObject(root, "error", error);
    return JsonString(root);
}

bool GetHttpQueryValue(httpd_req_t* req, const char* key, char* out, size_t out_len) {
    size_t query_len = httpd_req_get_url_query_len(req) + 1;
    if (query_len <= 1) {
        return false;
    }

    std::unique_ptr<char[]> query(new (std::nothrow) char[query_len]);
    if (!query) {
        return false;
    }

    return httpd_req_get_url_query_str(req, query.get(), query_len) == ESP_OK &&
        httpd_query_key_value(query.get(), key, out, out_len) == ESP_OK;
}

int GetHttpQueryInt(httpd_req_t* req, const char* key, int fallback, int min_value, int max_value) {
    char value[16] = {};
    if (!GetHttpQueryValue(req, key, value, sizeof(value))) {
        return fallback;
    }
    return std::clamp(atoi(value), min_value, max_value);
}
} // namespace

static std::string Base64Encode(const std::string& data) {
    size_t required = 0;
    size_t written = 0;
    mbedtls_base64_encode(nullptr, 0, &required,
        reinterpret_cast<const unsigned char*>(data.data()), data.size());
    std::string encoded(required, '\0');
    if (mbedtls_base64_encode(reinterpret_cast<unsigned char*>(encoded.data()), encoded.size(), &written,
        reinterpret_cast<const unsigned char*>(data.data()), data.size()) != 0) {
        return "";
    }
    encoded.resize(written);
    return encoded;
}

Esp32Camera::Esp32Camera(const camera_config_t &config) {
    base_config_ = config;
    esp_err_t err = esp_camera_init(&config);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "esp_camera_init failed with error 0x%x", err);
        return;
    }
    active_pixel_format_ = config.pixel_format;
    active_frame_size_ = config.frame_size;
    active_jpeg_quality_ = config.jpeg_quality;

    sensor_t *s = esp_camera_sensor_get();
    if (s) {
        if (s->id.PID == GC0308_PID) {
            hmirror_enabled_ = false;
        }
        ApplyOrientationLocked(s);
        ESP_LOGI(TAG, "Camera initialized: format=%d", config.pixel_format);
    }

    streaming_on_ = true;
    SetSensorProfileLocked(kDefaultStreamFrameSize, kStreamJpegQuality, "idle");
}

Esp32Camera::~Esp32Camera() {
    StopStream();
    if (lan_httpd_ != nullptr) {
        httpd_stop(lan_httpd_);
        lan_httpd_ = nullptr;
    }
    if (streaming_on_) {
        if (current_fb_) {
            esp_camera_fb_return(current_fb_);
            current_fb_ = nullptr;
        }
        ReleaseEncodeBufferLocked();
        esp_camera_deinit();
        streaming_on_ = false;
    }
}

void Esp32Camera::SetExplainUrl(const std::string &url, const std::string &token) {
    explain_url_ = url;
    explain_token_ = token;
}

bool Esp32Camera::Capture() {
    std::lock_guard<std::mutex> lock(camera_mutex_);
    if (!SetSensorProfileLocked(kPhotoFrameSize, kPhotoJpegQuality, "photo")) {
        return false;
    }
    return CaptureLocked(true);
}

void Esp32Camera::ReleaseCurrentFrameLocked() {
    if (current_fb_) {
        esp_camera_fb_return(current_fb_);
        current_fb_ = nullptr;
    }
}

void Esp32Camera::ReleaseEncodeBufferLocked() {
    if (encode_buf_) {
        heap_caps_free(encode_buf_);
        encode_buf_ = nullptr;
        encode_buf_size_ = 0;
    }
}

void Esp32Camera::ApplyOrientationLocked(sensor_t *sensor) {
    if (!sensor) {
        return;
    }
    sensor->set_hmirror(sensor, hmirror_enabled_ ? 1 : 0);
    sensor->set_vflip(sensor, vflip_enabled_ ? 1 : 0);
}

bool Esp32Camera::ReinitializeSensorLocked(pixformat_t pixel_format, framesize_t frame_size, int jpeg_quality, const char* reason) {
    if (encoder_thread_.joinable()) {
        encoder_thread_.join();
    }

    ReleaseCurrentFrameLocked();
    ReleaseEncodeBufferLocked();
    if (streaming_on_) {
        esp_camera_deinit();
        streaming_on_ = false;
    }

    camera_config_t config = base_config_;
    config.pixel_format = pixel_format;
    config.frame_size = frame_size;
    config.jpeg_quality = jpeg_quality;

    esp_err_t err = esp_camera_init(&config);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "Failed to reinitialize camera for %s: 0x%x", reason, err);
        return false;
    }

    streaming_on_ = true;
    active_pixel_format_ = config.pixel_format;
    active_frame_size_ = config.frame_size;
    active_jpeg_quality_ = config.jpeg_quality;

    sensor_t *sensor = esp_camera_sensor_get();
    ApplyOrientationLocked(sensor);
    ESP_LOGI(TAG, "Camera reinitialized: %s format=%d frame_size=%d jpeg_quality=%d",
             reason, pixel_format, frame_size, jpeg_quality);
    vTaskDelay(kSensorSettleDelay);
    return true;
}

bool Esp32Camera::RestoreIdleProfileLocked() {
    if (!streaming_on_ || active_pixel_format_ != base_config_.pixel_format) {
        if (!ReinitializeSensorLocked(base_config_.pixel_format, base_config_.frame_size, base_config_.jpeg_quality, "restore")) {
            return false;
        }
    }
    return SetSensorProfileLocked(kDefaultStreamFrameSize, kStreamJpegQuality, "idle");
}

bool Esp32Camera::SetSensorProfileLocked(framesize_t frame_size, int jpeg_quality, const char* reason, int settle_ms) {
    if (encoder_thread_.joinable()) {
        encoder_thread_.join();
    }

    if (!streaming_on_) {
        return false;
    }

    sensor_t *s = esp_camera_sensor_get();
    if (!s) {
        ESP_LOGE(TAG, "Camera sensor is not available");
        return false;
    }
    if (active_pixel_format_ != base_config_.pixel_format) {
        return ReinitializeSensorLocked(base_config_.pixel_format, frame_size, jpeg_quality, reason);
    }

    bool changed = false;
    if (active_frame_size_ != frame_size) {
        ReleaseCurrentFrameLocked();
        if (s->set_framesize(s, frame_size) != 0) {
            ESP_LOGE(TAG, "Failed to set camera frame size: %d", frame_size);
            return false;
        }
        active_frame_size_ = frame_size;
        changed = true;
    }

    if (active_jpeg_quality_ != jpeg_quality) {
        if (s->set_quality(s, jpeg_quality) != 0) {
            ESP_LOGE(TAG, "Failed to set camera JPEG quality: %d", jpeg_quality);
            return false;
        }
        active_jpeg_quality_ = jpeg_quality;
        changed = true;
    }

    if (changed) {
        ESP_LOGI(TAG, "Camera profile: %s frame_size=%d jpeg_quality=%d", reason, frame_size, jpeg_quality);
        int delay_ms = settle_ms >= 0 ? settle_ms : kSensorSettleMs;
        if (delay_ms > 0) {
            vTaskDelay(pdMS_TO_TICKS(delay_ms));
        }
    }
    return true;
}

bool Esp32Camera::CaptureLocked(bool update_preview, int discard_frames, bool log_frame) {
    if (encoder_thread_.joinable()) {
        encoder_thread_.join();
    }

    if (!streaming_on_) {
        return false;
    }

    // Get the latest frame, discard old frames for real-time performance.
    int reads = std::max(1, discard_frames + 1);
    for (int i = 0; i < reads; i++) {
        ReleaseCurrentFrameLocked();
        current_fb_ = esp_camera_fb_get();
        if (!current_fb_) {
            ESP_LOGE(TAG, "Camera capture failed");
            return false;
        }
    }

    // Prepare encode buffer for RGB565 format (with optional byte swapping)
    if (current_fb_->format == PIXFORMAT_RGB565) {
        size_t pixel_count = current_fb_->width * current_fb_->height;
        size_t data_size = pixel_count * 2;

        // Allocate or reallocate encode buffer if needed
        if (encode_buf_size_ < data_size) {
            if (encode_buf_) {
                heap_caps_free(encode_buf_);
            }
            encode_buf_ = (uint8_t *)heap_caps_malloc(data_size, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
            if (encode_buf_ == nullptr) {
                ESP_LOGE(TAG, "Failed to allocate memory for encode buffer");
                encode_buf_size_ = 0;
                return false;
            }
            encode_buf_size_ = data_size;
        }

        // Copy data to encode buffer with optional byte swapping
        uint16_t *src = (uint16_t *)current_fb_->buf;
        uint16_t *dst = (uint16_t *)encode_buf_;
        if (swap_bytes_enabled_) {
            for (size_t i = 0; i < pixel_count; i++) {
                dst[i] = __builtin_bswap16(src[i]);
            }
        } else {
            memcpy(encode_buf_, current_fb_->buf, data_size);
        }

        if (update_preview) {
            // Allocate separate buffer for preview display
            uint8_t *preview_data = (uint8_t *)heap_caps_malloc(data_size, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
            if (preview_data != nullptr) {
                memcpy(preview_data, encode_buf_, data_size);
                auto display = dynamic_cast<LvglDisplay *>(Board::GetInstance().GetDisplay());
                if (display != nullptr) {
                    display->SetPreviewImage(std::make_unique<LvglAllocatedImage>(preview_data, data_size, current_fb_->width, current_fb_->height, current_fb_->width * 2, LV_COLOR_FORMAT_RGB565));
                } else {
                    heap_caps_free(preview_data);
                }
            }
        }
    } else if (current_fb_->format == PIXFORMAT_JPEG) {
        // JPEG format preview usually requires decoding, skip preview display for now, just log
        ESP_LOGW(TAG, "JPEG capture success, len=%d, but not supported for preview", (int)current_fb_->len);
    }

    if (log_frame) {
        ESP_LOGI(TAG, "Captured frame: %dx%d, len=%d, format=%d",
                 current_fb_->width, current_fb_->height, (int)current_fb_->len, current_fb_->format);
    }

    return true;
}

bool Esp32Camera::SetHMirror(bool enabled) {
    hmirror_enabled_ = enabled;
    sensor_t *s = esp_camera_sensor_get();
    if (!s) {
        return false;
    }
    s->set_hmirror(s, enabled ? 1 : 0);
    return true;
}

bool Esp32Camera::SetVFlip(bool enabled) {
    vflip_enabled_ = enabled;
    sensor_t *s = esp_camera_sensor_get();
    if (!s) {
        return false;
    }
    s->set_vflip(s, enabled ? 1 : 0);
    return true;
}

bool Esp32Camera::SetSwapBytes(bool enabled) {
    swap_bytes_enabled_ = enabled;
    return true;
}

bool Esp32Camera::StartLanServer() {
    std::lock_guard<std::mutex> lock(camera_mutex_);
    return StartLanHttpServerLocked();
}

std::string Esp32Camera::Explain(const std::string &question) {
    if (explain_url_.empty()) {
        throw std::runtime_error("Image explain URL or token is not set");
    }

    std::lock_guard<std::mutex> lock(camera_mutex_);
    if (encoder_thread_.joinable()) {
        encoder_thread_.join();
    }

    if (current_fb_ == nullptr) {
        SetSensorProfileLocked(kDefaultStreamFrameSize, kStreamJpegQuality, "idle");
        throw std::runtime_error("No camera frame captured");
    }
    const int captured_width = current_fb_->width;
    const int captured_height = current_fb_->height;

    // Create local JPEG queue
    QueueHandle_t jpeg_queue = xQueueCreate(40, sizeof(JpegChunk));
    if (jpeg_queue == nullptr) {
        ESP_LOGE(TAG, "Failed to create JPEG queue");
        SetSensorProfileLocked(kDefaultStreamFrameSize, kStreamJpegQuality, "idle");
        throw std::runtime_error("Failed to create JPEG queue");
    }

    // Start encoding thread
    encoder_thread_ = std::thread([this, jpeg_queue]() {
        int64_t start_time = esp_timer_get_time();
        uint16_t w = current_fb_->width;
        uint16_t h = current_fb_->height;
        v4l2_pix_fmt_t enc_fmt;
        switch (current_fb_->format) {
            case PIXFORMAT_RGB565:
                enc_fmt = V4L2_PIX_FMT_RGB565;
                break;
            case PIXFORMAT_YUV422:
                enc_fmt = V4L2_PIX_FMT_YUYV;  // YUV422 is actually YUYV format
                break;
            case PIXFORMAT_YUV420:
                enc_fmt = V4L2_PIX_FMT_YUV420;
                break;
            case PIXFORMAT_GRAYSCALE:
                enc_fmt = V4L2_PIX_FMT_GREY;
                break;
            case PIXFORMAT_JPEG:
                enc_fmt = V4L2_PIX_FMT_JPEG;
                break;
            case PIXFORMAT_RGB888:
                enc_fmt = V4L2_PIX_FMT_RGB24;
                break;
            default:
                ESP_LOGE(TAG, "Unsupported pixel format: %d", current_fb_->format);
                return;
        }

        // Use encode buffer for RGB565, otherwise use original frame buffer
        uint8_t *jpeg_src_buf = current_fb_->buf;
        size_t jpeg_src_len = current_fb_->len;
        if (current_fb_->format == PIXFORMAT_RGB565 && encode_buf_ != nullptr) {
            jpeg_src_buf = encode_buf_;
            jpeg_src_len = encode_buf_size_;
        }

        if (current_fb_->format == PIXFORMAT_JPEG) {
            JpegChunk chunk = {.data = (uint8_t*)heap_caps_aligned_alloc(16, jpeg_src_len, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT), .len = jpeg_src_len};
            if (chunk.data == nullptr) {
                ESP_LOGE(TAG, "Failed to allocate %zu bytes for JPEG chunk", jpeg_src_len);
                chunk.len = 0;
            } else {
                memcpy(chunk.data, jpeg_src_buf, jpeg_src_len);
            }
            xQueueSend(jpeg_queue, &chunk, portMAX_DELAY);
            JpegChunk terminator = {.data = nullptr, .len = 0};
            xQueueSend(jpeg_queue, &terminator, portMAX_DELAY);
            ESP_LOGI(TAG, "JPEG passthrough size: %d", (int)jpeg_src_len);
            return;
        }

        bool ok = image_to_jpeg_cb(jpeg_src_buf, jpeg_src_len, w, h, enc_fmt, 80,
            [](void* arg, size_t index, const void* data, size_t len) -> size_t {
                auto jpeg_queue = static_cast<QueueHandle_t>(arg);
                JpegChunk chunk = {.data = nullptr, .len = len};
                if (index == 0 && data != nullptr && len > 0) {
                    chunk.data = (uint8_t*)heap_caps_aligned_alloc(16, len, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
                    if (chunk.data == nullptr) {
                        ESP_LOGE(TAG, "Failed to allocate %zu bytes for JPEG chunk", len);
                        chunk.len = 0;
                    } else {
                        memcpy(chunk.data, data, len);
                    }
                } else {
                    chunk.len = 0;  // Sentinel or error
                }
                xQueueSend(jpeg_queue, &chunk, portMAX_DELAY);
                return len;
            }, jpeg_queue);

        if (!ok) {
            JpegChunk chunk = {.data = nullptr, .len = 0};
            xQueueSend(jpeg_queue, &chunk, portMAX_DELAY);
        }
        int64_t end_time = esp_timer_get_time();
        ESP_LOGI(TAG, "JPEG encoding time: %ld ms", int((end_time - start_time) / 1000));
    });

    auto network = Board::GetInstance().GetNetwork();
    auto http = network->CreateHttp(3);
    std::string boundary = "----ESP32_CAMERA_BOUNDARY";

    http->SetHeader("Device-Id", SystemInfo::GetMacAddress().c_str());
    http->SetHeader("Client-Id", Board::GetInstance().GetUuid().c_str());
    if (!explain_token_.empty()) {
        http->SetHeader("Authorization", "Bearer " + explain_token_);
    }
    http->SetHeader("Content-Type", "multipart/form-data; boundary=" + boundary);
    http->SetHeader("Transfer-Encoding", "chunked");
    if (!http->Open("POST", explain_url_)) {
        ESP_LOGE(TAG, "Failed to connect to explain URL");
        encoder_thread_.join();
        JpegChunk chunk;
        while (xQueueReceive(jpeg_queue, &chunk, portMAX_DELAY) == pdPASS) {
            if (chunk.data != nullptr) {
                heap_caps_free(chunk.data);
            } else {
                break;
            }
        }
        vQueueDelete(jpeg_queue);
        SetSensorProfileLocked(kDefaultStreamFrameSize, kStreamJpegQuality, "idle");
        throw std::runtime_error("Failed to connect to explain URL");
    }

    {
        std::string question_field;
        question_field += "--" + boundary + "\r\n";
        question_field += "Content-Disposition: form-data; name=\"question\"\r\n";
        question_field += "\r\n";
        question_field += question + "\r\n";
        http->Write(question_field.c_str(), question_field.size());
    }
    {
        std::string file_header;
        file_header += "--" + boundary + "\r\n";
        file_header += "Content-Disposition: form-data; name=\"file\"; filename=\"camera.jpg\"\r\n";
        file_header += "Content-Type: image/jpeg\r\n";
        file_header += "\r\n";
        http->Write(file_header.c_str(), file_header.size());
    }

    size_t total_sent = 0;
    bool saw_terminator = false;
    while (true) {
        JpegChunk chunk;
        if (xQueueReceive(jpeg_queue, &chunk, portMAX_DELAY) != pdPASS) {
            ESP_LOGE(TAG, "Failed to receive JPEG chunk");
            break;
        }
        if (chunk.data == nullptr) {
            saw_terminator = true;
            break;
        }
        http->Write((const char *)chunk.data, chunk.len);
        total_sent += chunk.len;
        heap_caps_free(chunk.data);
    }
    encoder_thread_.join();
    vQueueDelete(jpeg_queue);

    if (!saw_terminator || total_sent == 0) {
        ESP_LOGE(TAG, "JPEG encoder failed or produced empty output");
        SetSensorProfileLocked(kDefaultStreamFrameSize, kStreamJpegQuality, "idle");
        throw std::runtime_error("Failed to encode image to JPEG");
    }

    {
        std::string multipart_footer;
        multipart_footer += "\r\n--" + boundary + "--\r\n";
        http->Write(multipart_footer.c_str(), multipart_footer.size());
    }
    http->Write("", 0);

    if (http->GetStatusCode() != 200) {
        ESP_LOGE(TAG, "Failed to upload photo, status code: %d", http->GetStatusCode());
        SetSensorProfileLocked(kDefaultStreamFrameSize, kStreamJpegQuality, "idle");
        throw std::runtime_error("Failed to upload photo");
    }

    std::string result = http->ReadAll();
    http->Close();

    size_t remain_stack_size = uxTaskGetStackHighWaterMark(nullptr);
    ESP_LOGI(TAG, "Explain image size=%dx%d, compressed size=%d, remain stack size=%d, question=%s\n%s",
             captured_width, captured_height, (int)total_sent, (int)remain_stack_size, question.c_str(), result.c_str());
    SetSensorProfileLocked(kDefaultStreamFrameSize, kStreamJpegQuality, "idle");
    return result;
}

std::string Esp32Camera::Snapshot(const std::string& resolution) {
    if (explain_url_.empty()) {
        return "{\"ok\":false,\"error\":\"image service is not configured\"}";
    }

    CameraSnapshotData snapshot;
    std::string error;
    if (!SnapshotToJpeg(resolution, CameraSnapshotOptions(), snapshot, error)) {
        return std::string("{\"ok\":false,\"error\":\"") + error + "\"}";
    }

    return UploadSnapshotLocked(snapshot.resolution, snapshot.body, snapshot.width, snapshot.height);
}

bool Esp32Camera::SnapshotToJpeg(const std::string& resolution, const CameraSnapshotOptions& options, CameraSnapshotData& snapshot, std::string& error) {
    error.clear();
    snapshot = CameraSnapshotData();

    const auto& selected = ResolveSnapshotResolution(resolution);
    sensor_t *sensor = esp_camera_sensor_get();
    if (!sensor) {
        error = "camera sensor is not available";
        return false;
    }
    camera_sensor_info_t *sensor_info = esp_camera_sensor_get_info(&sensor->id);
    if (sensor_info != nullptr && selected.frame_size > sensor_info->max_size) {
        error = "resolution not supported by sensor";
        return false;
    }

    int jpeg_quality = options.jpeg_quality >= 0 ? std::clamp(options.jpeg_quality, 0, 63) : selected.jpeg_quality;
    int settle_ms = options.settle_ms >= 0 ? std::clamp(options.settle_ms, 0, 3000) : kSensorSettleMs;
    int discard_frames = options.discard_frames >= 0 ? std::clamp(options.discard_frames, 0, 10) : kDefaultSnapshotDiscardFrames;

    std::lock_guard<std::mutex> lock(camera_mutex_);
    if (selected.reinitialize) {
        if (!ReinitializeSensorLocked(selected.pixel_format, selected.frame_size, jpeg_quality, "legacy snapshot")) {
            RestoreIdleProfileLocked();
            error = "failed to set legacy snapshot profile";
            return false;
        }
        if (settle_ms > kSensorSettleMs) {
            vTaskDelay(pdMS_TO_TICKS(settle_ms - kSensorSettleMs));
        }
    } else if (!SetSensorProfileLocked(selected.frame_size, jpeg_quality, "snapshot", settle_ms)) {
        RestoreIdleProfileLocked();
        error = "failed to set snapshot resolution";
        return false;
    }
    if (!CaptureLocked(false, discard_frames)) {
        RestoreIdleProfileLocked();
        error = "failed to capture snapshot";
        return false;
    }

    snapshot.resolution = selected.name;
    snapshot.width = current_fb_ ? static_cast<int>(current_fb_->width) : 0;
    snapshot.height = current_fb_ ? static_cast<int>(current_fb_->height) : 0;
    snapshot.jpeg_quality = jpeg_quality;
    snapshot.settle_ms = settle_ms;
    snapshot.discard_frames = discard_frames;
    if (!EncodeCurrentFrameToJpegLocked(snapshot.body, selected.encode_quality)) {
        RestoreIdleProfileLocked();
        error = "failed to encode snapshot";
        return false;
    }

    RestoreIdleProfileLocked();
    return true;
}

bool Esp32Camera::EncodeCurrentFrameToJpegLocked(std::string& jpeg_data, int quality) {
    if (current_fb_ == nullptr) {
        ESP_LOGE(TAG, "No frame available for JPEG encoding");
        return false;
    }

    jpeg_data.clear();
    if (current_fb_->format == PIXFORMAT_JPEG) {
        jpeg_data.assign(reinterpret_cast<const char*>(current_fb_->buf), current_fb_->len);
        return !jpeg_data.empty();
    }

    uint16_t w = current_fb_->width;
    uint16_t h = current_fb_->height;
    v4l2_pix_fmt_t enc_fmt;
    switch (current_fb_->format) {
        case PIXFORMAT_RGB565:
            enc_fmt = V4L2_PIX_FMT_RGB565;
            break;
        case PIXFORMAT_YUV422:
            enc_fmt = V4L2_PIX_FMT_YUYV;
            break;
        case PIXFORMAT_YUV420:
            enc_fmt = V4L2_PIX_FMT_YUV420;
            break;
        case PIXFORMAT_GRAYSCALE:
            enc_fmt = V4L2_PIX_FMT_GREY;
            break;
        case PIXFORMAT_RGB888:
            enc_fmt = V4L2_PIX_FMT_RGB24;
            break;
        default:
            ESP_LOGE(TAG, "Unsupported pixel format for streaming: %d", current_fb_->format);
            return false;
    }

    uint8_t *jpeg_src_buf = current_fb_->buf;
    size_t jpeg_src_len = current_fb_->len;
    if (current_fb_->format == PIXFORMAT_RGB565) {
        if (encode_buf_ == nullptr || encode_buf_size_ == 0) {
            ESP_LOGE(TAG, "RGB565 encode buffer is not ready");
            return false;
        }
        jpeg_src_buf = encode_buf_;
        jpeg_src_len = encode_buf_size_;
    }

    bool ok = image_to_jpeg_cb(jpeg_src_buf, jpeg_src_len, w, h, enc_fmt, quality,
        [](void* arg, size_t index, const void* data, size_t len) -> size_t {
            auto out = static_cast<std::string*>(arg);
            if (data != nullptr && len > 0) {
                out->append(static_cast<const char*>(data), len);
            }
            return len;
        }, &jpeg_data);

    if (!ok || jpeg_data.empty()) {
        ESP_LOGE(TAG, "Failed to encode stream frame to JPEG");
        return false;
    }
    return true;
}

std::string Esp32Camera::GetSnapshotUrl() const {
    auto marker = explain_url_.find("/mcp/vision/explain");
    if (marker != std::string::npos) {
        return explain_url_.substr(0, marker) + "/mcp/vision/snapshot";
    }

    auto slash = explain_url_.rfind('/');
    if (slash != std::string::npos) {
        return explain_url_.substr(0, slash + 1) + "snapshot";
    }
    return "";
}

std::string Esp32Camera::UploadSnapshotLocked(const std::string& resolution, const std::string& jpeg_data, int width, int height) {
    std::string snapshot_url = GetSnapshotUrl();
    if (snapshot_url.empty()) {
        return "{\"ok\":false,\"error\":\"snapshot URL is not configured\"}";
    }
    if (jpeg_data.empty()) {
        return "{\"ok\":false,\"error\":\"snapshot image is empty\"}";
    }

    auto network = Board::GetInstance().GetNetwork();
    auto http = network->CreateHttp(3);
    http->SetHeader("Device-Id", SystemInfo::GetMacAddress().c_str());
    http->SetHeader("Client-Id", Board::GetInstance().GetUuid().c_str());
    if (!explain_token_.empty()) {
        http->SetHeader("Authorization", "Bearer " + explain_token_);
    }
    http->SetHeader("Content-Type", "image/jpeg");
    http->SetHeader("Transfer-Encoding", "chunked");
    http->SetHeader("X-Xiaoli-Resolution", resolution);
    http->SetHeader("X-Xiaoli-Width", std::to_string(width));
    http->SetHeader("X-Xiaoli-Height", std::to_string(height));

    int64_t start_ms = esp_timer_get_time() / 1000;
    if (!http->Open("POST", snapshot_url)) {
        ESP_LOGE(TAG, "Failed to connect to snapshot URL");
        return "{\"ok\":false,\"error\":\"failed to connect to snapshot URL\"}";
    }
    http->Write(jpeg_data.data(), jpeg_data.size());
    http->Write("", 0);

    if (http->GetStatusCode() != 200) {
        ESP_LOGE(TAG, "Failed to upload snapshot, status code: %d", http->GetStatusCode());
        http->Close();
        return "{\"ok\":false,\"error\":\"failed to upload snapshot\"}";
    }
    std::string result = http->ReadAll();
    http->Close();

    int elapsed_ms = (int)(esp_timer_get_time() / 1000 - start_ms);
    ESP_LOGI(TAG, "Snapshot uploaded resolution=%s size=%dx%d jpeg=%d elapsed=%dms\n%s",
        resolution.c_str(), width, height, (int)jpeg_data.size(), elapsed_ms, result.c_str());
    return result;
}

std::string Esp32Camera::GetStreamUrl() const {
    auto marker = explain_url_.find("/mcp/vision/explain");
    if (marker != std::string::npos) {
        return explain_url_.substr(0, marker) + "/mcp/vision/stream/frame";
    }

    auto slash = explain_url_.rfind('/');
    if (slash != std::string::npos) {
        return explain_url_.substr(0, slash + 1) + "stream/frame";
    }
    return "";
}

bool Esp32Camera::UploadStreamFrame(const std::string& stream_id, int seq, const std::string& jpeg_data) {
    int64_t start_ms = esp_timer_get_time() / 1000;
    std::string encoded = Base64Encode(jpeg_data);
    if (encoded.empty()) {
        ESP_LOGE(TAG, "Failed to base64 encode stream frame");
        return false;
    }
    if (encoded.size() > 60000) {
        ESP_LOGW(TAG, "Dropping stream frame seq=%d because base64 payload is too large: %d", seq, (int)encoded.size());
        return false;
    }

    std::string payload;
    payload.reserve(encoded.size() + 256);
    payload += "{\"jsonrpc\":\"2.0\",\"method\":\"xiaoli/vision_frame\",\"params\":{";
    payload += "\"stream_id\":\"" + stream_id + "\",";
    payload += "\"seq\":" + std::to_string(seq) + ",";
    payload += "\"timestamp_ms\":" + std::to_string((long long)(esp_timer_get_time() / 1000)) + ",";
    payload += "\"mime_type\":\"image/jpeg\",";
    payload += "\"data\":\"" + encoded + "\"";
    payload += "}}";
    bool sent = Application::GetInstance().SendMcpMessageAndWait(payload, 5000);
    if (!sent) {
        ESP_LOGW(TAG, "Timed out or failed to publish stream frame seq=%d", seq);
        return false;
    }

    int elapsed_ms = (int)(esp_timer_get_time() / 1000 - start_ms);
    ESP_LOGI(TAG, "Published stream frame seq=%d jpeg=%d base64=%d elapsed=%dms",
        seq, (int)jpeg_data.size(), (int)encoded.size(), elapsed_ms);
    return true;
}

esp_err_t Esp32Camera::LanIndexHttpHandler(httpd_req_t* req) {
    return static_cast<Esp32Camera*>(req->user_ctx)->HandleLanIndex(req);
}

esp_err_t Esp32Camera::LanStatusHttpHandler(httpd_req_t* req) {
    return static_cast<Esp32Camera*>(req->user_ctx)->HandleLanStatus(req);
}

esp_err_t Esp32Camera::LanCaptureHttpHandler(httpd_req_t* req) {
    return static_cast<Esp32Camera*>(req->user_ctx)->HandleLanCapture(req);
}

esp_err_t Esp32Camera::LanStreamHttpHandler(httpd_req_t* req) {
    return static_cast<Esp32Camera*>(req->user_ctx)->HandleLanStream(req);
}

esp_err_t Esp32Camera::LanAudioTestCodecHttpHandler(httpd_req_t* req) {
    return static_cast<Esp32Camera*>(req->user_ctx)->HandleAudioTestCodec(req);
}

esp_err_t Esp32Camera::LanAudioTestServiceHttpHandler(httpd_req_t* req) {
    return static_cast<Esp32Camera*>(req->user_ctx)->HandleAudioTestService(req);
}

bool Esp32Camera::StartLanHttpServerLocked() {
    if (lan_httpd_ != nullptr) {
        return true;
    }

    httpd_config_t config = HTTPD_DEFAULT_CONFIG();
    config.server_port = XIAOLI_LAN_STREAM_PORT;
    config.ctrl_port = 32769;
    config.stack_size = 8192;
    config.max_uri_handlers = 8;

    esp_err_t err = httpd_start(&lan_httpd_, &config);
    if (err != ESP_OK) {
        lan_httpd_ = nullptr;
        ESP_LOGE(TAG, "Failed to start LAN camera HTTP server: %s", esp_err_to_name(err));
        return false;
    }

    httpd_uri_t index_uri = {};
    index_uri.uri = "/";
    index_uri.method = HTTP_GET;
    index_uri.handler = LanIndexHttpHandler;
    index_uri.user_ctx = this;
    httpd_register_uri_handler(lan_httpd_, &index_uri);

    httpd_uri_t status_uri = {};
    status_uri.uri = "/status";
    status_uri.method = HTTP_GET;
    status_uri.handler = LanStatusHttpHandler;
    status_uri.user_ctx = this;
    httpd_register_uri_handler(lan_httpd_, &status_uri);

    httpd_uri_t capture_uri = {};
    capture_uri.uri = "/capture";
    capture_uri.method = HTTP_GET;
    capture_uri.handler = LanCaptureHttpHandler;
    capture_uri.user_ctx = this;
    httpd_register_uri_handler(lan_httpd_, &capture_uri);

    httpd_uri_t stream_uri = {};
    stream_uri.uri = "/stream";
    stream_uri.method = HTTP_GET;
    stream_uri.handler = LanStreamHttpHandler;
    stream_uri.user_ctx = this;
    httpd_register_uri_handler(lan_httpd_, &stream_uri);

    httpd_uri_t test_codec_uri = {};
    test_codec_uri.uri = "/test_codec";
    test_codec_uri.method = HTTP_GET;
    test_codec_uri.handler = LanAudioTestCodecHttpHandler;
    test_codec_uri.user_ctx = this;
    httpd_register_uri_handler(lan_httpd_, &test_codec_uri);

    httpd_uri_t test_service_uri = {};
    test_service_uri.uri = "/test_service";
    test_service_uri.method = HTTP_GET;
    test_service_uri.handler = LanAudioTestServiceHttpHandler;
    test_service_uri.user_ctx = this;
    httpd_register_uri_handler(lan_httpd_, &test_service_uri);

    ESP_LOGI(TAG, "LAN camera HTTP server started on port %d", XIAOLI_LAN_STREAM_PORT);
    return true;
}

std::string Esp32Camera::GetLanBaseUrlLocked() const {
    auto& wifi = WifiManager::GetInstance();
    if (!wifi.IsConnected()) {
        return "";
    }
    std::string ip = wifi.GetIpAddress();
    if (ip.empty() || ip == "0.0.0.0") {
        return "";
    }
    return "http://" + ip + ":" + std::to_string(XIAOLI_LAN_STREAM_PORT);
}

std::string Esp32Camera::GetLanStreamUrlLocked() const {
    std::string base_url = GetLanBaseUrlLocked();
    return base_url.empty() ? "" : base_url + "/stream";
}

std::string Esp32Camera::GetLanCaptureUrlLocked() const {
    std::string base_url = GetLanBaseUrlLocked();
    return base_url.empty() ? "" : base_url + "/capture";
}

std::string Esp32Camera::StartLanStreamLocked(const std::string& resolution) {
    if (!StartLanHttpServerLocked()) {
        return JsonError("failed to start LAN camera HTTP server");
    }

    std::string base_url = GetLanBaseUrlLocked();
    if (base_url.empty()) {
        return JsonError("WiFi station IP is not available");
    }

    const auto& stream_resolution = ResolveStreamResolution(resolution);
    lan_stream_resolution_ = stream_resolution.name;
    lan_stream_frame_size_ = stream_resolution.frame_size;
    lan_stream_jpeg_quality_ = kLanStreamJpegQuality;
    lan_stream_active_ = true;

    if (!SetSensorProfileLocked(lan_stream_frame_size_, lan_stream_jpeg_quality_, "lan_stream", 0)) {
        lan_stream_active_ = false;
        return JsonError("failed to set LAN stream camera profile");
    }

    Board::GetInstance().SetPowerSaveLevel(PowerSaveLevel::PERFORMANCE);
    std::string stream_url = GetLanStreamUrlLocked();
    std::string capture_url = GetLanCaptureUrlLocked();

    cJSON* root = cJSON_CreateObject();
    cJSON_AddBoolToObject(root, "ok", true);
    cJSON_AddStringToObject(root, "transport", "lan");
    cJSON_AddStringToObject(root, "resolution", lan_stream_resolution_.c_str());
    cJSON_AddStringToObject(root, "page_url", base_url.c_str());
    cJSON_AddStringToObject(root, "mjpeg_url", stream_url.c_str());
    cJSON_AddStringToObject(root, "capture_url", capture_url.c_str());
    cJSON_AddNumberToObject(root, "port", XIAOLI_LAN_STREAM_PORT);
    return JsonString(root);
}

esp_err_t Esp32Camera::HandleLanIndex(httpd_req_t* req) {
    static const char html[] =
        "<!doctype html><html><head><meta charset=\"utf-8\">"
        "<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">"
        "<title>Xiaoli Camera</title>"
        "<style>body{margin:0;background:#111;color:#eee;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif}"
        "header{display:flex;gap:8px;align-items:center;flex-wrap:wrap;padding:10px;background:#202124}"
        "button,select,input{height:32px;background:#2c2d30;color:#eee;border:1px solid #555;border-radius:4px;padding:0 8px}"
        "main{display:grid;place-items:center;min-height:calc(100vh - 54px)}"
        "img{max-width:100vw;max-height:calc(100vh - 54px);background:#000}</style></head>"
        "<body><header><select id=\"res\"><option value=\"qqvga\">QQVGA</option><option value=\"qvga\">QVGA</option>"
        "<option value=\"vga\" selected>VGA</option><option value=\"svga\">SVGA</option></select>"
        "<label>JPEG <input id=\"quality\" type=\"number\" min=\"4\" max=\"40\" value=\"12\"></label>"
        "<button onclick=\"apply()\">Apply</button><button onclick=\"capture()\">Capture</button>"
        "<button onclick=\"fetch('/test_codec').then(r=>r.json()).then(d=>alert(d.ok?'Codec OK':'FAIL'))\" style=\"background:#0a6\">Test Codec</button>"
        "<button onclick=\"fetch('/test_service').then(r=>r.json()).then(d=>alert(d.ok?'Service OK':'FAIL'))\" style=\"background:#60a\">Test AudioService</button></header>"
        "<main><img id=\"stream\"></main><script>"
        "const img=document.getElementById('stream');"
        "function url(){const r=document.getElementById('res').value,q=document.getElementById('quality').value;"
        "return '/stream?resolution='+encodeURIComponent(r)+'&quality='+encodeURIComponent(q)+'&ts='+Date.now();}"
        "function apply(){img.src='';setTimeout(()=>img.src=url(),120)}"
        "function capture(){window.open('/capture?resolution='+encodeURIComponent(document.getElementById('res').value),'_blank')}"
        "apply();</script></body></html>";
    httpd_resp_set_type(req, "text/html");
    httpd_resp_set_hdr(req, "Access-Control-Allow-Origin", "*");
    return httpd_resp_send(req, html, HTTPD_RESP_USE_STRLEN);
}

esp_err_t Esp32Camera::HandleLanStatus(httpd_req_t* req) {
    cJSON* root = cJSON_CreateObject();
    cJSON_AddBoolToObject(root, "ok", true);
    cJSON_AddBoolToObject(root, "active", lan_stream_active_);
    cJSON_AddStringToObject(root, "resolution", lan_stream_resolution_.c_str());
    cJSON_AddStringToObject(root, "mjpeg_url", GetLanStreamUrlLocked().c_str());
    std::string body = JsonString(root);
    httpd_resp_set_type(req, "application/json");
    httpd_resp_set_hdr(req, "Access-Control-Allow-Origin", "*");
    return httpd_resp_send(req, body.c_str(), body.size());
}

esp_err_t Esp32Camera::HandleLanCapture(httpd_req_t* req) {
    char resolution[16] = {};
    std::string requested_resolution = GetHttpQueryValue(req, "resolution", resolution, sizeof(resolution)) ? resolution : lan_stream_resolution_;

    CameraSnapshotOptions options;
    options.jpeg_quality = GetHttpQueryInt(req, "quality", kLanStreamJpegQuality, 0, 63);
    options.settle_ms = GetHttpQueryInt(req, "settle_ms", kSensorSettleMs, 0, 3000);
    options.discard_frames = GetHttpQueryInt(req, "discard_frames", 1, 0, 10);

    CameraSnapshotData snapshot;
    std::string error;
    if (!SnapshotToJpeg(requested_resolution, options, snapshot, error)) {
        httpd_resp_set_status(req, "500 Internal Server Error");
        return httpd_resp_sendstr(req, error.c_str());
    }

    httpd_resp_set_type(req, "image/jpeg");
    httpd_resp_set_hdr(req, "Content-Disposition", "inline; filename=xiaoli.jpg");
    httpd_resp_set_hdr(req, "Access-Control-Allow-Origin", "*");
    return httpd_resp_send(req, snapshot.body.data(), snapshot.body.size());
}

esp_err_t Esp32Camera::HandleLanStream(httpd_req_t* req) {
    char resolution[16] = {};
    if (GetHttpQueryValue(req, "resolution", resolution, sizeof(resolution))) {
        StreamResolution stream_resolution = ResolveStreamResolution(std::string(resolution));
        std::lock_guard<std::mutex> lock(camera_mutex_);
        lan_stream_resolution_ = stream_resolution.name;
        lan_stream_frame_size_ = stream_resolution.frame_size;
    }
    int requested_quality = GetHttpQueryInt(req, "quality", lan_stream_jpeg_quality_, 4, 40);
    {
        std::lock_guard<std::mutex> lock(camera_mutex_);
        lan_stream_jpeg_quality_ = requested_quality;
        lan_stream_active_ = true;
        SetSensorProfileLocked(lan_stream_frame_size_, lan_stream_jpeg_quality_, "lan_stream", 0);
    }

    esp_err_t result = httpd_resp_set_type(req, kLanStreamContentType);
    if (result != ESP_OK) {
        return result;
    }
    httpd_resp_set_hdr(req, "Access-Control-Allow-Origin", "*");
    httpd_resp_set_hdr(req, "Cache-Control", "no-store");

    char part_header[96] = {};
    while (lan_stream_active_) {
        std::string jpeg_data;
        bool frame_ok = false;
        {
            std::lock_guard<std::mutex> lock(camera_mutex_);
            if (SetSensorProfileLocked(lan_stream_frame_size_, lan_stream_jpeg_quality_, "lan_stream", 0) &&
                CaptureLocked(false, 0, false)) {
                frame_ok = EncodeCurrentFrameToJpegLocked(jpeg_data, 80);
            }
        }

        if (!frame_ok || jpeg_data.empty()) {
            vTaskDelay(pdMS_TO_TICKS(20));
            continue;
        }

        size_t part_len = snprintf(part_header, sizeof(part_header), kLanStreamPartHeader,
            static_cast<unsigned int>(jpeg_data.size()));
        result = httpd_resp_send_chunk(req, kLanStreamBoundaryLine, strlen(kLanStreamBoundaryLine));
        if (result == ESP_OK) {
            result = httpd_resp_send_chunk(req, part_header, part_len);
        }
        if (result == ESP_OK) {
            result = httpd_resp_send_chunk(req, jpeg_data.data(), jpeg_data.size());
        }
        if (result != ESP_OK) {
            break;
        }
        vTaskDelay(pdMS_TO_TICKS(1));
    }

    return result;
}

esp_err_t Esp32Camera::HandleAudioTestCodec(httpd_req_t* req) {
    ESP_LOGI(TAG, "Test Codec: starting...");
    auto codec = Board::GetInstance().GetAudioCodec();
    if (!codec) {
        ESP_LOGE(TAG, "Test Codec: codec is null");
        httpd_resp_set_type(req, "application/json");
        httpd_resp_set_hdr(req, "Access-Control-Allow-Origin", "*");
        return httpd_resp_send(req, "{\"ok\":false,\"error\":\"codec is null\"}", HTTPD_RESP_USE_STRLEN);
    }

    ESP_LOGI(TAG, "Test Codec: sample_rate=%d enabled_in=%d enabled_out=%d",
        codec->output_sample_rate(), codec->input_enabled(), codec->output_enabled());

    codec->Start();
    codec->EnableOutput(true);

    ESP_LOGI(TAG, "Test Codec: after enable out=%d", codec->output_enabled());

    int sample_rate = codec->output_sample_rate();
    if (sample_rate <= 0) {
        ESP_LOGW(TAG, "Test Codec: sample_rate=%d, defaulting to 8000", sample_rate);
        sample_rate = 8000;
    }
    int duration_ms = 500;
    int frequency = 1000;
    int amplitude = 20000;
    int total_samples = sample_rate * duration_ms / 1000;

    ESP_LOGI(TAG, "Test Codec: generating %d samples at %d Hz", total_samples, sample_rate);

    std::vector<int16_t> pcm(total_samples);
    for (int i = 0; i < total_samples; ++i) {
        double phase = 2.0 * M_PI * frequency * i / sample_rate;
        pcm[i] = static_cast<int16_t>(std::sin(phase) * amplitude);
    }

    ESP_LOGI(TAG, "Test Codec: calling OutputData...");
    codec->OutputData(pcm);
    ESP_LOGI(TAG, "Test Codec: done");

    httpd_resp_set_type(req, "application/json");
    httpd_resp_set_hdr(req, "Access-Control-Allow-Origin", "*");
    return httpd_resp_send(req, "{\"ok\":true,\"test\":\"codec\"}", HTTPD_RESP_USE_STRLEN);
}

esp_err_t Esp32Camera::HandleAudioTestService(httpd_req_t* req) {
    ESP_LOGI(TAG, "Test Service: playing OGG_SUCCESS...");
    Application::GetInstance().GetAudioService().PlaySound(Lang::Sounds::OGG_SUCCESS);
    ESP_LOGI(TAG, "Test Service: done");
    httpd_resp_set_type(req, "application/json");
    httpd_resp_set_hdr(req, "Access-Control-Allow-Origin", "*");
    return httpd_resp_send(req, "{\"ok\":true,\"test\":\"service\"}", HTTPD_RESP_USE_STRLEN);
}

void Esp32Camera::StreamLoop(std::string stream_id, int fps, int duration_sec, framesize_t frame_size, std::string resolution) {
    ESP_LOGI(TAG, "Starting camera stream: id=%s resolution=%s fps=%d duration=%d",
        stream_id.c_str(), resolution.c_str(), fps, duration_sec);
    auto& board = Board::GetInstance();
    board.SetPowerSaveLevel(PowerSaveLevel::PERFORMANCE);
    const int frame_interval_ms = 1000 / fps;
    const int64_t start_ms = esp_timer_get_time() / 1000;
    const int64_t duration_ms = static_cast<int64_t>(duration_sec) * 1000;
    int seq = 0;

    while (!stream_stop_requested_) {
        int64_t now_ms = esp_timer_get_time() / 1000;
        if (now_ms - start_ms >= duration_ms) {
            break;
        }

        int64_t frame_start_ms = now_ms;
        std::string jpeg_data;
        bool frame_ok = false;
        {
            std::lock_guard<std::mutex> lock(camera_mutex_);
            if (SetSensorProfileLocked(frame_size, kStreamJpegQuality, "stream") && CaptureLocked(false)) {
                frame_ok = EncodeCurrentFrameToJpegLocked(jpeg_data, 60);
            }
        }

        if (frame_ok) {
            UploadStreamFrame(stream_id, seq++, jpeg_data);
        }

        int64_t elapsed_ms = esp_timer_get_time() / 1000 - frame_start_ms;
        int delay_ms = frame_interval_ms - static_cast<int>(elapsed_ms);
        if (delay_ms > 0) {
            vTaskDelay(pdMS_TO_TICKS(delay_ms));
        } else {
            vTaskDelay(pdMS_TO_TICKS(1));
        }
    }

    stream_running_ = false;
    stream_stop_requested_ = false;
    board.SetPowerSaveLevel(PowerSaveLevel::LOW_POWER);
    ESP_LOGI(TAG, "Camera stream stopped: id=%s frames=%d", stream_id.c_str(), seq);
}

std::string Esp32Camera::StartStream(int fps, int duration_sec, const std::string& resolution, const std::string& transport) {
    if (IsLanStreamTransport(transport) || IsAutoStreamTransport(transport)) {
        std::string lan_result;
        {
            std::lock_guard<std::mutex> lock(camera_mutex_);
            lan_result = StartLanStreamLocked(resolution);
        }
        if (!IsAutoStreamTransport(transport) || lan_result.find("\"ok\":true") != std::string::npos) {
            return lan_result;
        }
        ESP_LOGW(TAG, "LAN stream is unavailable, falling back to remote stream: %s", lan_result.c_str());
    }

    if (explain_url_.empty()) {
        return "{\"ok\":false,\"error\":\"image service is not configured\"}";
    }

    lan_stream_active_ = false;
    fps = std::max(1, std::min(fps, 3));
    duration_sec = std::max(1, std::min(duration_sec, 60));
    const auto& stream_resolution = ResolveStreamResolution(resolution);

    if (stream_running_) {
        return "{\"ok\":false,\"error\":\"stream already running\"}";
    }
    if (stream_task_handle_ != nullptr) {
        return "{\"ok\":false,\"error\":\"stream task is still stopping\"}";
    }

    stream_running_ = true;
    stream_stop_requested_ = false;

    std::string stream_id = SystemInfo::GetMacAddress() + "-" + std::to_string((long long)esp_timer_get_time());
    std::replace(stream_id.begin(), stream_id.end(), ':', '-');

    auto args = std::unique_ptr<StreamTaskArgs>(new (std::nothrow) StreamTaskArgs{
        this,
        stream_id,
        fps,
        duration_sec,
        stream_resolution.frame_size,
        stream_resolution.name,
    });
    if (!args) {
        stream_running_ = false;
        return "{\"ok\":false,\"error\":\"failed to allocate stream task\"}";
    }

    auto ret = xTaskCreate(
        [](void* raw) {
            std::unique_ptr<StreamTaskArgs> args(static_cast<StreamTaskArgs*>(raw));
            auto camera = args->camera;
            camera->StreamLoop(args->stream_id, args->fps, args->duration_sec, args->frame_size, args->resolution);
            camera->stream_task_handle_ = nullptr;
            vTaskDelete(nullptr);
        },
        "CameraStream",
        kStreamTaskStackSize,
        args.get(),
        kStreamTaskPriority,
        &stream_task_handle_);
    if (ret != pdPASS) {
        stream_task_handle_ = nullptr;
        stream_running_ = false;
        stream_stop_requested_ = false;
        return "{\"ok\":false,\"error\":\"failed to start stream task\"}";
    }
    args.release();

    return "{\"ok\":true,\"transport\":\"remote\",\"stream_id\":\"" + stream_id + "\",\"fps\":" + std::to_string(fps) +
        ",\"duration_sec\":" + std::to_string(duration_sec) +
        ",\"resolution\":\"" + stream_resolution.name + "\"}";
}

bool Esp32Camera::StopStream() {
    bool had_lan_stream = lan_stream_active_;
    lan_stream_active_ = false;
    stream_stop_requested_ = true;
    const int64_t start_ms = esp_timer_get_time() / 1000;
    while (stream_running_) {
        if ((esp_timer_get_time() / 1000) - start_ms > 10000) {
            ESP_LOGW(TAG, "Timed out waiting for camera stream task to stop");
            return false;
        }
        vTaskDelay(pdMS_TO_TICKS(50));
    }
    stream_task_handle_ = nullptr;
    stream_stop_requested_ = false;
    if (had_lan_stream) {
        Board::GetInstance().SetPowerSaveLevel(PowerSaveLevel::LOW_POWER);
    }
    return true;
}
