#include "serial_hwtest.h"
#include "sdkconfig.h"

#if CONFIG_XIAOLI_SERIAL_HWTEST

#include <algorithm>
#include <cJSON.h>
#include <cmath>
#include <cstdarg>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <memory>
#include <string>
#include <string_view>
#include <vector>
#include <unistd.h>

#include <esp_heap_caps.h>
#include <esp_log.h>
#include <freertos/FreeRTOS.h>
#include <freertos/task.h>
#include <mbedtls/base64.h>

#include "application.h"
#include "assets/lang_config.h"
#include "audio_codec.h"
#include "board.h"
#include "camera.h"

#define TAG "SerialHwtest"

namespace {
constexpr char kCommandPrefix[] = "XIAOLI_TEST ";
constexpr char kResultPrefix[] = "XIAOLI_TEST_RESULT ";
constexpr char kDataPrefix[] = "XIAOLI_TEST_DATA ";
constexpr char kEndPrefix[] = "XIAOLI_TEST_END ";
constexpr size_t kLineMax = CONFIG_XIAOLI_SERIAL_HWTEST_LINE_MAX;
constexpr size_t kSnapshotChunkBytes = CONFIG_XIAOLI_SERIAL_HWTEST_SNAPSHOT_CHUNK_BYTES;
constexpr size_t kAudioMaxBytes = 512 * 1024;
constexpr int kSnapshotYieldEveryChunks = 1;
TaskHandle_t g_task_handle = nullptr;
std::string g_audio_id;
std::string g_audio_buffer;
size_t g_audio_expected_bytes = 0;

int SilentLogVprintf(const char*, va_list) {
    return 0;
}

class ScopedLogSilencer {
public:
    ScopedLogSilencer() : previous_vprintf_(esp_log_set_vprintf(&SilentLogVprintf)) {}
    ~ScopedLogSilencer() {
        esp_log_set_vprintf(previous_vprintf_);
    }

    ScopedLogSilencer(const ScopedLogSilencer&) = delete;
    ScopedLogSilencer& operator=(const ScopedLogSilencer&) = delete;

private:
    vprintf_like_t previous_vprintf_;
};

const char* FindCommandJson(char* line) {
    const char* command = nullptr;
    char* search = line;
    while (true) {
        char* found = strstr(search, kCommandPrefix);
        if (found == nullptr) {
            break;
        }
        command = found + strlen(kCommandPrefix);
        search = found + 1;
    }
    if (command == nullptr) {
        return nullptr;
    }
    return strchr(command, '{');
}

std::string JsonEscape(const std::string& value) {
    std::string out;
    out.reserve(value.size() + 8);
    for (char ch : value) {
        switch (ch) {
        case '\\':
            out += "\\\\";
            break;
        case '"':
            out += "\\\"";
            break;
        case '\n':
            out += "\\n";
            break;
        case '\r':
            out += "\\r";
            break;
        case '\t':
            out += "\\t";
            break;
        default:
            out += ch;
            break;
        }
    }
    return out;
}

std::string IdOf(cJSON* root) {
    auto id = cJSON_GetObjectItem(root, "id");
    if (cJSON_IsString(id)) {
        return id->valuestring;
    }
    if (cJSON_IsNumber(id)) {
        return std::to_string(id->valueint);
    }
    return "";
}

std::string StringField(cJSON* root, const char* name, const char* fallback = "") {
    auto item = cJSON_GetObjectItem(root, name);
    if (cJSON_IsString(item)) {
        return item->valuestring;
    }
    return fallback;
}

int IntField(cJSON* root, const char* name, int fallback = 0) {
    auto item = cJSON_GetObjectItem(root, name);
    if (cJSON_IsNumber(item)) {
        return item->valueint;
    }
    return fallback;
}

void PrintJsonLine(const char* prefix, const std::string& payload) {
    printf("%s%s\n", prefix, payload.c_str());
    fflush(stdout);
}

void PrintOk(const std::string& id, const std::string& extra = "") {
    std::string payload = "{\"id\":\"" + JsonEscape(id) + "\",\"ok\":true";
    if (!extra.empty()) {
        payload += ",";
        payload += extra;
    }
    payload += "}";
    PrintJsonLine(kResultPrefix, payload);
}

void PrintError(const std::string& id, const std::string& error) {
    PrintJsonLine(kResultPrefix,
        "{\"id\":\"" + JsonEscape(id) + "\",\"ok\":false,\"error\":\"" + JsonEscape(error) + "\"}");
}

std::string Base64Encode(const uint8_t* data, size_t len) {
    size_t required = 0;
    size_t written = 0;
    mbedtls_base64_encode(nullptr, 0, &required, data, len);
    std::string encoded(required, '\0');
    if (mbedtls_base64_encode(reinterpret_cast<unsigned char*>(encoded.data()), encoded.size(), &written,
            data, len) != 0) {
        return "";
    }
    encoded.resize(written);
    return encoded;
}

bool Base64DecodeAppend(const std::string& encoded, std::string& out) {
    size_t required = 0;
    size_t written = 0;
    int probe = mbedtls_base64_decode(nullptr, 0, &required,
        reinterpret_cast<const unsigned char*>(encoded.data()), encoded.size());
    if (probe != 0 && probe != MBEDTLS_ERR_BASE64_BUFFER_TOO_SMALL) {
        return false;
    }
    std::string decoded(required, '\0');
    if (mbedtls_base64_decode(reinterpret_cast<unsigned char*>(decoded.data()), decoded.size(), &written,
            reinterpret_cast<const unsigned char*>(encoded.data()), encoded.size()) != 0) {
        return false;
    }
    decoded.resize(written);
    out.append(decoded);
    return true;
}

std::string_view BuiltinSound(const std::string& name) {
    if (name == "success") return Lang::Sounds::OGG_SUCCESS;
    if (name == "popup") return Lang::Sounds::OGG_POPUP;
    if (name == "exclamation") return Lang::Sounds::OGG_EXCLAMATION;
    if (name == "activation") return Lang::Sounds::OGG_ACTIVATION;
    if (name == "upgrade") return Lang::Sounds::OGG_UPGRADE;
    if (name == "vibration") return Lang::Sounds::OGG_VIBRATION;
    if (name == "welcome") return Lang::Sounds::OGG_WELCOME;
    if (name == "wificonfig") return Lang::Sounds::OGG_WIFICONFIG;
    if (name == "low_battery") return Lang::Sounds::OGG_LOW_BATTERY;
    if (name == "err_pin") return Lang::Sounds::OGG_ERR_PIN;
    if (name == "err_reg") return Lang::Sounds::OGG_ERR_REG;
    if (name == "0" || name == "digit_0") return Lang::Sounds::OGG_0;
    if (name == "1" || name == "digit_1") return Lang::Sounds::OGG_1;
    if (name == "2" || name == "digit_2") return Lang::Sounds::OGG_2;
    if (name == "3" || name == "digit_3") return Lang::Sounds::OGG_3;
    if (name == "4" || name == "digit_4") return Lang::Sounds::OGG_4;
    if (name == "5" || name == "digit_5") return Lang::Sounds::OGG_5;
    if (name == "6" || name == "digit_6") return Lang::Sounds::OGG_6;
    if (name == "7" || name == "digit_7") return Lang::Sounds::OGG_7;
    if (name == "8" || name == "digit_8") return Lang::Sounds::OGG_8;
    if (name == "9" || name == "digit_9") return Lang::Sounds::OGG_9;
    return {};
}

void HandleSnapshot(const std::string& id, cJSON* root) {
    auto camera = Board::GetInstance().GetCamera();
    if (camera == nullptr) {
        PrintError(id, "camera is not available");
        return;
    }

    std::string resolution = StringField(root, "resolution", "vga");
    CameraSnapshotOptions options;
    options.jpeg_quality = IntField(root, "quality", -1);
    options.settle_ms = IntField(root, "settle_ms", -1);
    options.discard_frames = IntField(root, "discard_frames", -1);
    PrintOk(id, "\"cmd\":\"snapshot\",\"stage\":\"accepted\",\"resolution\":\"" + JsonEscape(resolution) + "\"");

    CameraSnapshotData snapshot;
    std::string error;
    if (!camera->SnapshotToJpeg(resolution, options, snapshot, error)) {
        PrintError(id, error);
        return;
    }

    PrintJsonLine(kResultPrefix,
        "{\"id\":\"" + JsonEscape(id) +
        "\",\"ok\":true,\"cmd\":\"snapshot\",\"stage\":\"captured\",\"content_type\":\"" + snapshot.content_type +
        "\",\"resolution\":\"" + JsonEscape(snapshot.resolution) +
        "\",\"width\":" + std::to_string(snapshot.width) +
        ",\"height\":" + std::to_string(snapshot.height) +
        ",\"quality\":" + std::to_string(snapshot.jpeg_quality) +
        ",\"settle_ms\":" + std::to_string(snapshot.settle_ms) +
        ",\"discard_frames\":" + std::to_string(snapshot.discard_frames) +
        ",\"length\":" + std::to_string(snapshot.body.size()) +
        ",\"encoding\":\"base64\"}");

    ScopedLogSilencer silence_logs;
    int seq = 0;
    for (size_t offset = 0; offset < snapshot.body.size(); offset += kSnapshotChunkBytes) {
        size_t len = std::min(kSnapshotChunkBytes, snapshot.body.size() - offset);
        std::string encoded = Base64Encode(reinterpret_cast<const uint8_t*>(snapshot.body.data() + offset), len);
        if (encoded.empty()) {
            PrintError(id, "failed to base64 encode snapshot");
            return;
        }
        PrintJsonLine(kDataPrefix,
            "{\"id\":\"" + JsonEscape(id) + "\",\"seq\":" + std::to_string(seq++) +
            ",\"data\":\"" + encoded + "\"}");
        if ((seq % kSnapshotYieldEveryChunks) == 0) {
            vTaskDelay(pdMS_TO_TICKS(1));
        }
    }
    PrintJsonLine(kEndPrefix, "{\"id\":\"" + JsonEscape(id) + "\",\"ok\":true}");
}

void HandlePlaySound(const std::string& id, cJSON* root) {
    std::string name = StringField(root, "name", "success");
    auto sound = BuiltinSound(name);
    if (sound.empty()) {
        PrintError(id, "unknown built-in sound");
        return;
    }
    Application::GetInstance().Schedule([sound]() {
        Application::GetInstance().PlaySound(sound);
    });
    PrintOk(id, "\"cmd\":\"play_sound\",\"name\":\"" + JsonEscape(name) + "\"");
}

void HandleStatus(const std::string& id) {
    auto codec = Board::GetInstance().GetAudioCodec();
    if (codec == nullptr) {
        PrintError(id, "audio codec is not available");
        return;
    }
    PrintOk(id,
        "\"cmd\":\"status\",\"audio_speaker\":{\"volume\":" + std::to_string(codec->output_volume()) +
        ",\"output_enabled\":" + std::string(codec->output_enabled() ? "true" : "false") +
        ",\"sample_rate\":" + std::to_string(codec->output_sample_rate()) + "}");
}

void HandleSetVolume(const std::string& id, cJSON* root) {
    auto codec = Board::GetInstance().GetAudioCodec();
    if (codec == nullptr) {
        PrintError(id, "audio codec is not available");
        return;
    }
    int volume = std::clamp(IntField(root, "volume", 100), 0, 100);
    codec->SetOutputVolume(volume);
    PrintOk(id,
        "\"cmd\":\"set_volume\",\"audio_speaker\":{\"volume\":" + std::to_string(codec->output_volume()) + "}");
}

void HandlePlayOggBegin(const std::string& id, cJSON* root) {
    int length = IntField(root, "length", 0);
    if (length <= 0 || static_cast<size_t>(length) > kAudioMaxBytes) {
        PrintError(id, "invalid audio length");
        return;
    }
    g_audio_id = id;
    g_audio_expected_bytes = static_cast<size_t>(length);
    g_audio_buffer.clear();
    g_audio_buffer.reserve(g_audio_expected_bytes);
    PrintOk(id, "\"cmd\":\"play_ogg_begin\",\"max_bytes\":" + std::to_string(kAudioMaxBytes));
}

void HandlePlayOggChunk(const std::string& id, cJSON* root) {
    if (g_audio_id.empty() || id != g_audio_id) {
        PrintError(id, "audio transfer is not open");
        return;
    }
    std::string data = StringField(root, "data");
    if (data.empty() || !Base64DecodeAppend(data, g_audio_buffer)) {
        PrintError(id, "invalid audio chunk");
        return;
    }
    if (g_audio_buffer.size() > g_audio_expected_bytes || g_audio_buffer.size() > kAudioMaxBytes) {
        g_audio_id.clear();
        g_audio_buffer.clear();
        PrintError(id, "audio transfer is too large");
        return;
    }
    PrintOk(id, "\"cmd\":\"play_ogg_chunk\",\"received\":" + std::to_string(g_audio_buffer.size()));
}

void HandlePlayOggEnd(const std::string& id) {
    if (g_audio_id.empty() || id != g_audio_id) {
        PrintError(id, "audio transfer is not open");
        return;
    }
    if (g_audio_buffer.empty() || g_audio_buffer.size() != g_audio_expected_bytes) {
        PrintError(id, "audio transfer length mismatch");
        return;
    }
    auto audio = std::make_shared<std::string>(std::move(g_audio_buffer));
    g_audio_id.clear();
    g_audio_expected_bytes = 0;
    Application::GetInstance().Schedule([audio]() {
        Application::GetInstance().PlaySound(std::string_view(audio->data(), audio->size()));
    });
    PrintOk(id, "\"cmd\":\"play_ogg\",\"length\":" + std::to_string(audio->size()));
}

void HandleCommand(cJSON* root) {
    std::string id = IdOf(root);
    std::string cmd = StringField(root, "cmd");
    if (id.empty()) {
        id = "unknown";
    }
    if (cmd == "ping") {
        PrintOk(id, "\"cmd\":\"ping\"");
    } else if (cmd == "status") {
        HandleStatus(id);
    } else if (cmd == "set_volume") {
        HandleSetVolume(id, root);
    } else if (cmd == "snapshot") {
        HandleSnapshot(id, root);
    } else if (cmd == "play_sound") {
        HandlePlaySound(id, root);
    } else if (cmd == "play_ogg_begin") {
        HandlePlayOggBegin(id, root);
    } else if (cmd == "play_ogg_chunk") {
        HandlePlayOggChunk(id, root);
    } else if (cmd == "play_ogg_end") {
        HandlePlayOggEnd(id);
    } else {
        PrintError(id, "unknown command");
    }
}

void ProcessCommandLine(char* line, size_t len) {
    while (len > 0 && (line[len - 1] == '\n' || line[len - 1] == '\r')) {
        line[--len] = '\0';
    }
    const char* json = FindCommandJson(line);
    if (json == nullptr) {
        return;
    }
    const char* parse_end = nullptr;
    cJSON* root = cJSON_ParseWithOpts(json, &parse_end, false);
    if (root == nullptr) {
        PrintError("unknown", "invalid json");
        return;
    }
    if (!cJSON_IsObject(root)) {
        cJSON_Delete(root);
        PrintError("unknown", "command must be an object");
        return;
    }
    HandleCommand(root);
    cJSON_Delete(root);
}

void SerialHwtestTask(void*) {
    ESP_LOGI(TAG, "Serial hardware test task started");
    auto line = static_cast<char*>(heap_caps_malloc(kLineMax, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT));
    if (line == nullptr) {
        ESP_LOGE(TAG, "Failed to allocate serial line buffer");
        g_task_handle = nullptr;
        vTaskDelete(nullptr);
        return;
    }

    size_t line_len = 0;
    char rx[128];
    while (true) {
        ssize_t received = read(fileno(stdin), rx, sizeof(rx));
        if (received <= 0) {
            vTaskDelay(pdMS_TO_TICKS(20));
            continue;
        }

        for (ssize_t i = 0; i < received; ++i) {
            char ch = rx[i];
            if (ch == '\r') {
                continue;
            }
            if (ch == '\n') {
                if (line_len > 0) {
                    line[line_len] = '\0';
                    ProcessCommandLine(line, line_len);
                    line_len = 0;
                }
                continue;
            }
            if (line_len < kLineMax - 1) {
                line[line_len++] = ch;
            } else {
                line_len = 0;
                PrintError("unknown", "command line too long");
            }
        }
    }
}
} // namespace

void StartSerialHwtest() {
    if (g_task_handle != nullptr) {
        return;
    }
    xTaskCreate(SerialHwtestTask, "serial_hwtest", CONFIG_XIAOLI_SERIAL_HWTEST_TASK_STACK_SIZE,
        nullptr, CONFIG_XIAOLI_SERIAL_HWTEST_TASK_PRIORITY, &g_task_handle);
}

#else

void StartSerialHwtest() {}

#endif
