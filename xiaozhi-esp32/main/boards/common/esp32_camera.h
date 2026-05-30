#pragma once
#include "sdkconfig.h"

#include <lvgl.h>
#include <atomic>
#include <thread>
#include <memory>
#include <mutex>
#include <vector>

#include <freertos/FreeRTOS.h>
#include <freertos/queue.h>
#include <freertos/task.h>
#include <esp_http_server.h>

#include "camera.h"
#include "esp_camera.h"
#include "jpg/image_to_jpeg.h"

struct JpegChunk
{
    uint8_t *data;
    size_t len;
};

class Esp32Camera : public Camera
{
private:
    bool streaming_on_ = false;
    bool swap_bytes_enabled_ = true;  // Swap pixel byte order for RGB565, enabled by default
    camera_config_t base_config_ = {};
    std::string explain_url_;
    std::string explain_token_;
    std::thread encoder_thread_;
    TaskHandle_t stream_task_handle_ = nullptr;
    std::atomic<bool> stream_running_ = false;
    std::atomic<bool> stream_stop_requested_ = false;
    std::atomic<bool> lan_stream_active_ = false;
    std::mutex camera_mutex_;
    httpd_handle_t lan_httpd_ = nullptr;
    camera_fb_t *current_fb_ = nullptr;
    uint8_t *encode_buf_ = nullptr;  // Buffer for JPEG encoding (with optional byte swap)
    size_t encode_buf_size_ = 0;
    pixformat_t active_pixel_format_ = PIXFORMAT_JPEG;
    framesize_t active_frame_size_ = FRAMESIZE_QVGA;
    int active_jpeg_quality_ = 30;
    framesize_t lan_stream_frame_size_ = FRAMESIZE_VGA;
    int lan_stream_jpeg_quality_ = 12;
    std::string lan_stream_resolution_ = "vga";
    bool hmirror_enabled_ = false;
    bool vflip_enabled_ = false;

    void ReleaseCurrentFrameLocked();
    void ReleaseEncodeBufferLocked();
    void ApplyOrientationLocked(sensor_t *sensor);
    bool ReinitializeSensorLocked(pixformat_t pixel_format, framesize_t frame_size, int jpeg_quality, const char* reason);
    bool RestoreIdleProfileLocked();
    bool SetSensorProfileLocked(framesize_t frame_size, int jpeg_quality, const char* reason, int settle_ms = -1);
    bool CaptureLocked(bool update_preview, int discard_frames = 1, bool log_frame = true);
    bool EncodeCurrentFrameToJpegLocked(std::string& jpeg_data, int quality);
    std::string UploadSnapshotLocked(const std::string& resolution, const std::string& jpeg_data, int width, int height);
    bool UploadStreamFrame(const std::string& stream_id, int seq, const std::string& jpeg_data);
    void StreamLoop(std::string stream_id, int fps, int duration_sec, framesize_t frame_size, std::string resolution);
    std::string GetSnapshotUrl() const;
    std::string GetStreamUrl() const;
    bool StartLanHttpServerLocked();
    std::string StartLanStreamLocked(const std::string& resolution);
    std::string GetLanBaseUrlLocked() const;
    std::string GetLanStreamUrlLocked() const;
    std::string GetLanCaptureUrlLocked() const;
    esp_err_t HandleLanIndex(httpd_req_t* req);
    esp_err_t HandleLanStatus(httpd_req_t* req);
    esp_err_t HandleLanCapture(httpd_req_t* req);
    esp_err_t HandleLanStream(httpd_req_t* req);
    esp_err_t HandleAudioTestCodec(httpd_req_t* req);
    esp_err_t HandleAudioTestService(httpd_req_t* req);
    static esp_err_t LanIndexHttpHandler(httpd_req_t* req);
    static esp_err_t LanStatusHttpHandler(httpd_req_t* req);
    static esp_err_t LanCaptureHttpHandler(httpd_req_t* req);
    static esp_err_t LanStreamHttpHandler(httpd_req_t* req);
    static esp_err_t LanAudioTestCodecHttpHandler(httpd_req_t* req);
    static esp_err_t LanAudioTestServiceHttpHandler(httpd_req_t* req);

public:
    Esp32Camera(const camera_config_t &config);
    ~Esp32Camera();

    virtual void SetExplainUrl(const std::string &url, const std::string &token) override;
    virtual bool Capture() override;
    virtual bool SetHMirror(bool enabled) override;
    virtual bool SetVFlip(bool enabled) override;
    virtual bool SetSwapBytes(bool enabled) override;
    virtual bool StartLanServer() override;
    virtual std::string Explain(const std::string &question) override;
    virtual std::string Snapshot(const std::string& resolution) override;
    virtual bool SnapshotToJpeg(const std::string& resolution, const CameraSnapshotOptions& options, CameraSnapshotData& snapshot, std::string& error) override;
    virtual std::string StartStream(int fps, int duration_sec, const std::string& resolution, const std::string& transport) override;
    virtual bool StopStream() override;
};
