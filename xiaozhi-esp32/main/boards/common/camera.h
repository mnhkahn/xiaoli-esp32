#ifndef CAMERA_H
#define CAMERA_H

#include <string>

struct CameraSnapshotData {
    std::string resolution;
    int width = 0;
    int height = 0;
    int jpeg_quality = 0;
    int settle_ms = 0;
    int discard_frames = 0;
    std::string content_type = "image/jpeg";
    std::string body;
};

struct CameraSnapshotOptions {
    int jpeg_quality = -1;
    int settle_ms = -1;
    int discard_frames = -1;
};

class Camera {
public:
    virtual void SetExplainUrl(const std::string& url, const std::string& token) = 0;
    virtual bool Capture() = 0;
    virtual bool SetHMirror(bool enabled) = 0;
    virtual bool SetVFlip(bool enabled) = 0;
    virtual bool SetSwapBytes(bool enabled) { return false; }  // Optional, default no-op
    virtual bool StartLanServer() { return false; }
    virtual std::string Explain(const std::string& question) = 0;
    virtual std::string Snapshot(const std::string& resolution) { return "{\"ok\":false,\"error\":\"snapshot not supported\"}"; }
    virtual bool SnapshotToJpeg(const std::string& resolution, CameraSnapshotData& snapshot, std::string& error) {
        return SnapshotToJpeg(resolution, CameraSnapshotOptions(), snapshot, error);
    }
    virtual bool SnapshotToJpeg(const std::string& resolution, const CameraSnapshotOptions& options, CameraSnapshotData& snapshot, std::string& error) {
        error = "snapshot not supported";
        return false;
    }
    virtual std::string StartStream(int fps, int duration_sec, const std::string& resolution, const std::string& transport) { return "{\"ok\":false,\"error\":\"stream not supported\"}"; }
    virtual std::string StartStream(int fps, int duration_sec, const std::string& resolution) { return StartStream(fps, duration_sec, resolution, "remote"); }
    virtual std::string StartStream(int fps, int duration_sec) { return StartStream(fps, duration_sec, "qvga", "remote"); }
    virtual bool StopStream() { return false; }
};

#endif // CAMERA_H
