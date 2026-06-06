# xiaoli-mac

Mac 端的"板子固件" — Go 重写 ESP32 上的 `xiaozhi-esp32`，1:1 复刻协议、状态机、显示逻辑，把硬件相关的部分替换成 Mac 原生实现：

| ESP32 | Mac |
| --- | --- |
| LVGL 显示屏 | Fyne 窗口（笑脸 + 状态 + 聊天气泡） |
| I2S 麦克风 | PortAudio |
| I2S 扬声器 | PortAudio |
| 本地 Wi-Fi 配网 | 不需要（用配置文件） |
| AFE 唤醒 | Porcupine / energy-VAD（已实现） |
| `display/lcd_display.cc` | `display/fynegui/display.go` |
| `device_state_machine.cc` | `state/machine.go` |
| `application.cc:OnIncomingJson` | `protocol/dispatcher.go` |
| `audio_service.cc` | `audio/{capture,playback,pipeline}.go` |
| `mcp_server.cc` | `mcp/server.go` + `mcp/tools/` |

## 目录

```
xiaoli-mac/
├── cmd/xiaoli-mac/      # 入口
├── app/                  # 事件循环 + 装配
├── config/               # JSON 配置
├── state/                # 状态机（11 状态 / 26 转移规则）
├── display/
│   ├── display.go        # Display interface (1:1 抄 ESP32)
│   └── fynegui/          # Fyne 实现
├── protocol/
│   ├── messages.go       # Envelope, AudioParams
│   ├── dispatcher.go     # 1:1 抄 application.cc:635-747
│   ├── transport/        # 通用 WebSocket 客户端
│   └── client/           # 设备协议 (SendHello/Listen/Audio/Abort/MCP)
├── assets/               # i18n (zh-CN, en-US)
├── audio/                # PortAudio + Opus (3 goroutine)
├── wakeword/             # energy-VAD / Porcupine 桩
├── mcp/                  # JSON-RPC 2.0 server
│   ├── server.go
│   └── tools/device.go   # get_device_status / set_volume / play_sound
├── uuid_darwin.go        # ioreg → IOPlatformUUID
└── xiaoli-mac.example.json
```

## 运行

```bash
brew install portaudio   # 首次需要
cd xiaoli-mac
cp xiaoli-mac.example.json xiaoli-mac.json
# 改 server/auth 后
CGO_ENABLED=1 go run ./cmd/xiaoli-mac -config xiaoli-mac.json
```

## 当前状态（全部完成）

- [x] **状态机**（1:1 抄 ESP32, 26 个转移 case 单测全过）
- [x] **Display interface**（1:1 抄 ESP32）
- [x] **Fyne 窗口**（笑脸 emoji / 状态圆点 / 聊天气泡 / 通知 toast）
- [x] **协议分发器**（1:1 抄 `application.cc:635-747`）
- [x] **主事件循环**（`Submit()` 等价 ESP32 `Schedule()`）
- [x] **WebSocket 客户端**（自动重连、ping/pong、`OnConnect` 钩子）
- [x] **设备协议层**（`SendHello`、`SendListenStart/Stop`、`SendAudio`、`SendAbort`、`SendMCP`）
- [x] **hello 握手** → 状态机从 `Connecting` 转 `Idle`
- [x] **音频管线**（PortAudio 采麦 + 播音 + Opus 32kbps VoIP，60ms 帧）
- [x] **TTS 解码**（`tts.start` 打开播放 + 解码循环；`tts.stop` 关闭）
- [x] **mic gate**（`Listening` 状态开启 mic 编码 + `listen.start`）
- [x] **本地 MCP server**（JSON-RPC 2.0，3 个 device tool）
- [x] **唤醒词**（energy-VAD 默认；Porcupine 桩）
- [x] **ioreg → IOPlatformUUID** 当 DeviceID
- [x] **单 goroutine 主循环**保证显示/状态/音频操作串行

## 测试

```bash
CGO_ENABLED=1 go test ./...
```

各包测试结果：

| 包 | 覆盖点 |
| --- | --- |
| `state` | 26 个状态转移 + 监听器回调 |
| `protocol` | 6 种消息分发 + system.reboot |
| `protocol/transport` | echo server、OnConnect 钩子 |
| `protocol/client` | hello 握手、SendListen/SendAudio/空包拒绝 |
| `audio` | Opus 编码-解码 round-trip、listen gate、loop 退出 |
| `mcp` | initialize / tools/list / unknown tool / notification |
| `mcp/tools` | get_device_status、set_volume 范围裁剪、play_sound |
| `wakeword` | energy trigger、静默不触发、engine 路由、RMS |

binary 体积 30MB（Fyne 自带 OpenGL 绑定）。
