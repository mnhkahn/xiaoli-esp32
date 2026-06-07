# TTS 协议与音频管线

服务端从上游 TTS 拿到 Ogg Opus，到 ESP32 设备扬声器发声，整条链路的设计、协议、算法、事件语义、状态机影响。

**本文档的"客户端"特指 ESP32 设备**。Mac 客户端 (`xiaoli-mac/`) 是同协议的桌面端参考实现，详见最后一节「参考实现」。

## 全景

```
┌──────────────┐   Ogg Opus    ┌────────────────┐   WS Text+Binary   ┌──────────┐   I2S PCM   ┌────────┐
│  SiliconFlow │ ────────────→ │  Go admin 服务  │ ─────────────────→ │  ESP32   │ ─────────→ │ 扬声器 │
│  /v1/audio/  │  ~20KB/句     │  parse+reencode │                    │ decode+play│            │        │
│  speech      │               │  +prebuffer     │                    │ +状态机    │            │        │
└──────────────┘               └────────────────┘                    └──────────┘            └────────┘
                                       │                                     │
                                       │ 同一 WebSocket 双向：                 │ 设备→服务
                                       │  设备 Binary = mic Opus            │  Text JSON
                                       │  设备 Text   = listen/abort/mcp    │
```

## 目录

1. [上游 TTS 调用与 Ogg 格式](#1-上游-tts-调用与-ogg-格式)
2. [Ogg → 60ms Opus 转换](#2-ogg--60ms-opus-转换)
3. [WebSocket 帧序列](#3-websocket-帧序列)
4. [Prebuffer + Pacing](#4-prebuffer--pacing)
5. [ESP32 接收与播放](#5-esp32-接收与播放)
6. [通道模型：会话 vs 通道](#6-通道模型会话-vs-通道)
7. [事件清单与设备反应](#7-事件清单与设备反应)
8. [emotion 字段详解](#8-emotion-字段详解)
9. [状态机影响总结](#9-状态机影响总结)
10. [与老 Python 服务端的关系](#10-与老-python-服务端的关系)
11. [参考实现：Mac 客户端](#11-参考实现mac-客户端)
12. [关键文件位置](#12-关键文件位置)
13. [外部参考仓库](#13-外部参考仓库)

---

## 1. 上游 TTS 调用与 Ogg 格式

这一节覆盖**服务端往外看的第一段**：怎么拉上游 TTS，拿回什么格式的数据。后续 [§2](#2-ogg--60ms-opus-转换) 才进入设备侧的转换。

### 1.1 HTTP 调用

`server/internal/admin/direct_tts.go:46-90`：

```json
POST https://api.siliconflow.cn/v1/audio/speech
{
  "model":           "FunAudioLLM/CosyVoice2-0.5B",
  "voice":           "FunAudioLLM/CosyVoice2-0.5B:anna",
  "input":           "你好宝宝",
  "response_format": "opus"           ← 强制 opus
}
```

服务端正则检查（`direct_tts.go:50-52`）：只接受 `opus` / `ogg`，否则报错。

### 1.2 返回的 Ogg 容器格式

响应体是个 Ogg 文件（`"OggS"` 开头）。**Ogg 不是编码，是容器**（类似 MP4/MKV 之于视频），Opus in Ogg = "Ogg Opus"。

```
[Page 1]  BOS flag=0x02, seq=0
  27B header  → "OggS" ver=0 type=BOS granule=0 serial=0x7869616f
  1B lacing   → [19]
  19B payload → "OpusHead"(8) ver=1 channels=1 rate=16000

[Page 2]  seq=1
  27B header  → "OggS" type=0 granule=0
  NB payload → "OpusTags" vendor="xiaoli-go"

[Page 3..N]  音频包
  27B header  → "OggS" type=0 granule=N*2880 seq=N
  NB lacing   → [...]
  NB payload → 单个 Opus packet

[Page M]  EOS flag=0x04
```

Ogg 页 27 字节头：

| 偏移 | 长度 | 含义 |
|---|---|---|
| 0 | 4 | 魔数 "OggS" |
| 4 | 1 | 版本（始终 0） |
| 5 | 1 | header_type（0x02=BOS, 0x04=EOS, 0x00=中间） |
| 6 | 8 | granule position（48kHz 累计采样） |
| 14 | 4 | serial number（0x7869616f = "xiao" 字节序） |
| 18 | 4 | page sequence |
| 22 | 4 | CRC32 |
| 26 | 1 | segment 数 |
| 27+ | N | lacing table（每段长度，255 表示"续"） |

Lacing table 编码：segment 长度 ≥ 255 时写 255 表示「继续」，最后一段 < 255 表示「本 Opus 包结束」。

---

## 2. Ogg → 60ms Opus 转换

上游 TTS 吐回 Ogg Opus 文件（`response_format=opus`），设备端 `esp_opus_dec` 期望**独立的 60ms Opus 单包**（`xiaozhi-esp32/main/audio/audio_service.h:39 #define OPUS_FRAME_DURATION_MS 60`）。Ogg 容器 + 变长帧 = 设备吃不进。

所以这一步只干一件事：**把上游的 Ogg Opus 转换成设备能直接吃的 60ms Opus 单包**。拆成两个动作做：

1. **拆 Ogg 容器**（`direct_ogg.go:181-240` `extractOpusPackets`）—— 拿到裸 Opus 包序列
2. **重编码为 60ms**（`direct_ogg.go:256-313` `reencodeOpusFrames`）—— 统一帧长

### 2.1 拆 Ogg 容器

按 Ogg 协议解析页结构（详见 [§1.2](#12-返回的-ogg-容器格式)）：

1. 读 27 字节头，校验 `"OggS"`
2. 读 N 字节 lacing table
3. 走 lacing：累加到 `partial`，碰到 `lacing < 255` 就当作一个 Opus 包切出
4. 跳过 `OpusHead` / `OpusTags`（前 8 字节魔数判断）
5. granule 累加，最后推算 `frameDuration = lastGranule/48000/packetCount`

输出：变长 Opus 包序列（10/20/40/60ms 不一）+ 平均帧时长。

### 2.2 重编码为 60ms

**为什么不解包直接传？** 变长帧喂设备会报错或 PCM 长度对不齐。

做法：decode→encode 一次有损转换（仅改帧长，音质影响小）：

```go
// 1. 全部包解码到 PCM int16 buffer（16kHz/mono）
dec, _ := opus.NewDecoder(16000, 1)
for _, pkt := range packets {
    n, _ := dec.Decode(pkt, buf)   // buf=1920 samples (120ms 上限)
    pcm = append(pcm, buf[:n]...)
}

// 2. 按 960 samples (60ms) 切片重编码
enc, _ := opus.NewEncoder(16000, 1, opus.AppVoIP)
for i := 0; i < len(pcm); i += 960 {
    frame := pcm[i:i+960]
    if len(frame) < 960 { pad(silence) }    // 最后一帧补零
    n, _ := enc.Encode(frame, encoded)
    out = append(out, encoded[:n])
}
```

输出：等长 60ms Opus 包序列。设备拿到就能一个 Binary 帧一帧地解码。

---

## 3. 服务端发送：帧序列与节拍

服务端发 TTS 是个**单循环**（`server/internal/admin/direct_device.go:465-540`），拆成两件事讲：

- **发什么 / 什么顺序** → [§3.1](#31-帧序列)
- **按什么节拍** → [§3.2](#32-节拍prebuffer--pacing)

### 3.1 帧序列

```
[Text]  {"type":"llm","emotion":"neutral"}
[Text]  {"type":"tts","state":"start","session_id":"xxx"}
[Text]  {"type":"tts","state":"sentence_start","text":"...","session_id":"xxx"}
[Binary] Opus packet 1 (60ms)        ← prebuffer：不节拍连发 5 个
[Binary] Opus packet 2
[Binary] Opus packet 3
[Binary] Opus packet 4
[Binary] Opus packet 5                ← 第 5 个发完才开始计时
[Binary] Opus packet 6                ← 此后每 60ms 一个
[Binary] Opus packet 7
...
[Text]  {"type":"tts","state":"stop","session_id":"xxx"}    ← defer
```

WebSocket 协议层（`direct_ws.go:16-22`）：

- `0x1` Text
- `0x2` Binary
- `0x8` Close
- `0x9` Ping
- `0xa` Pong

握手阶段服务端 hello 响应（`direct_device.go:295-307`）告知音频参数：

```json
{
  "type":"hello", "transport":"websocket", "version":1,
  "session_id":"xxx",
  "audio_params":{
    "format":"opus", "sample_rate":16000, "channels":1, "frame_duration":60
  }
}
```

### 3.2 节拍：Prebuffer + Pacing

`direct_device.go:512-540`：

```go
const assistantAudioPrebufferPacketNum = 5  // 预缓冲 5 帧

for i, pkt := range reencoded {
    if i == 5 {
        pacedStart = time.Now()              // 记下开始节拍的时间
    } else if deadline := pacedStart.Add((i-5) * 60ms); !deadline.IsZero() {
        waitUntil(ctx, deadline)             // 睡到节拍时间点
    }
    writeFrame(Binary, pkt)                  // 准时发
}
```

设计动机：
- **前 5 包不节拍**（连发）：让设备把解码队列灌满，避免播到一半空
- **从第 6 包起 60ms 节拍**：跟设备解码器节奏对齐，PCM 不会堆也不会断

关键常量（`direct_device.go:20-23`）：

```go
directDeviceAudioSampleRate      = 16000
directDeviceAudioChannels        = 1
directDeviceAudioFrameDurationMS = 60
assistantAudioPrebufferPacketNum = 5
```

---

## 4. ESP32 接收与播放

### 4.1 WebSocket 接收

`xiaozhi-esp32/main/protocols/websocket_protocol.cc:112-146`

```cpp
websocket_->OnData([this](const char* data, size_t len, bool binary) {
    if (binary) {
        // 每个 Binary 帧 = 一个 Opus packet
        on_incoming_audio_(make_unique<AudioStreamPacket>({
            .sample_rate = 16000,
            .frame_duration = 60,
            .payload = vector<uint8_t>(data, data+len)
        }));
    } else {
        // Text JSON：type=hello 协商参数；type=tts 更新播放状态；type=llm 改表情
    }
});
```

握手阶段 ESP32 发的 hello（`websocket_protocol.cc:203-225`）：

```json
{
  "type":"hello",
  "version":1,
  "transport":"websocket",
  "features":{"aec":true,"mcp":true},
  "audio_params":{"format":"opus","sample_rate":16000,"channels":1,"frame_duration":60}
}
```

### 4.2 解码 + 播放

`xiaozhi-esp32/main/audio/audio_service.cc:335-394`

```cpp
// 解码线程：从 audio_decode_queue_ 取 Opus 包
esp_opus_dec_decode(opus_decoder_, raw_in, pcm_out, &info);
// pcm 灌进 audio_playback_queue_
// 播放线程：消费 PCM，I2S 推给 codec (ES8311/ES8388 等)
```

`audio_service.h:39-43`：
```cpp
#define OPUS_FRAME_DURATION_MS 60
#define MAX_DECODE_PACKETS_IN_QUEUE (2400 / OPUS_FRAME_DURATION_MS)  // 40
#define MAX_SEND_PACKETS_IN_QUEUE   (2400 / OPUS_FRAME_DURATION_MS)  // 40
```

**packet-driven**：每个 Binary 帧独立解码一帧 60ms PCM，不依赖 Ogg 容器——这就是为什么服务端要 reencode + 拆成单包单帧。

### 4.3 设备反向流（mic 上行 + 控制）

同一个 WebSocket 上，设备还会上行：

| 设备→服务端 | 触发 | 用途 |
|---|---|---|
| Binary (Opus) | 持续 | mic 录音上行 |
| `{"type":"listen","state":"detect","text":"唤醒词"}` | 唤醒词命中 | 告诉服务端开始听 |
| `{"type":"listen","state":"start",...}` | 设备主动进入聆听 | 同上 |
| `{"type":"listen","state":"stop","text":"..."}` | VAD 切停 | 触发服务端 LLM |
| `{"type":"abort","reason":"wake_word_detected"}` | 唤醒词打断 TTS | 取消当前 TTS 播放 |
| `{"type":"mcp","payload":{...}}` | 设备发起工具调用 | MCP 工具调用 |

发送实现在 `xiaozhi-esp32/main/protocols/protocol.cc:42-79`。

---

## 5. 通道模型：会话 vs 通道

**没有单独的 TTS 通道**。设备跟服务端共用一个 WebSocket，从 `hello` 握手后**永久复用**：

```
设备上电
  ↓
WSS /xiaozhi/v1/ 建链
  ↓
[Text] →  {"type":"hello","audio_params":{...}}         ← 设备发
[Text] ←  {"type":"hello","session_id":"xxx",...}       ← 服务端回
  ↓
同一个 WebSocket 持续双向流：
  - 设备→服务端：Binary = mic 录音 Opus
  - 服务端→设备：Text = 控制事件 / Binary = TTS Opus
```

TTS 走的是同一通道上的**多次会话**，靠 `tts.start` / `tts.stop` 划界，不是连接级。`session_id` 标记单次 TTS 会话（`direct_device.go:471`）。

---

## 6. 事件清单与设备反应

### 6.1 `llm` 事件

`server/internal/admin/direct_device.go:470`

```json
{"type":"llm","emotion":"neutral"}
```

**当前服务端硬编码 `neutral`**。LLM 实际没参与。

| 字段 | 含义 | 设备端反应 |
|---|---|---|
| `emotion` | 表情标签字符串 | 屏幕表情图标（`SetEmotion`） |

**只影响表情，不影响任何状态机/音频**。详见 [§7](#7-emotion-字段详解)。

### 6.2 `tts` 事件

#### `state: start`

`server/internal/admin/direct_device.go:471`

```json
{"type":"tts","state":"start","session_id":"xxx"}
```

**触发 Speaking 状态**。设备端（`xiaozhi-esp32/main/application.cc:644-651`）：

```cpp
if (strcmp(state->valuestring, "start") == 0) {
    Schedule([this]() {
        aborted_ = false;
        SetDeviceState(kDeviceStateSpeaking);
    });
}
```

副作用（`application.cc:1114-1123`）：

| 影响项 | 值 |
|---|---|
| 状态文字 | `SPEAKING` |
| 状态机 | `Speaking`（`Idle`/`Listening` → `Speaking`） |
| AEC | **off**（扬声器出声，不再回采） |
| 唤醒词 | realtime 模式保留，其他关 |
| **解码队列** | **reset**（清空上一次的尾巴） |
| Mic 上行 | 停 |

#### `state: sentence_start`

`server/internal/admin/direct_device.go:472`

```json
{"type":"tts","state":"sentence_start","text":"你好宝宝","session_id":"xxx"}
```

**不影响状态机**。设备端（`application.cc:665-676`）只更新聊天框文本：

```cpp
auto text = cJSON_GetObjectItem(root, "text");
if (cJSON_IsString(text)) {
    Schedule([this, display, message = std::string(text->valuestring)]() {
        display->SetChatMessage("assistant", message.c_str());
    });
}
```

#### `state: stop`

`server/internal/admin/direct_device.go:474`，**defer**

```json
{"type":"tts","state":"stop","session_id":"xxx"}
```

`stop` 是 `defer`，**所有 Opus 包发完才触发**。

设备端（`application.cc:652-664`）：

```cpp
if (GetDeviceState() == kDeviceStateSpeaking) {
    if (listening_mode_ == kListeningModeManualStop) {
        SetDeviceState(kDeviceStateIdle);       // 屏幕：STANDBY
    } else {
        SetDeviceState(kDeviceStateListening);  // 屏幕：LISTENING
    }
}
```

副作用：

| 影响项 | 值 |
|---|---|
| 状态文字 | `STANDBY` / `LISTENING`（**立即切**） |
| 状态机 | `Speaking` → `Idle` 或 `Listening` |
| AEC | on（mic 重新开始上行） |
| Mic 上行 | 开 |
| **解码队列** | **不动**（`audio_playback_queue_` 继续排空中） |

### 6.3 二进制 Opus 包

每个 Binary 帧 = 一个 60ms Opus packet，独立解码。详见 [§3](#3-服务端发送帧序列与节拍)。

---

## 7. emotion 字段详解

### 10.1 是什么

`llm.emotion` 是个**表情标签字符串**，用来告诉设备「TTS 期间在屏幕上显示什么表情图标」。

完整映射表（`xiaozhi-esp32/main/display/lvgl_display/emoji_collection.cc:54-74` 32px / `:102+` 64px）：

| 字符串 | Emoji | 字符串 | Emoji |
|---|---|---|---|
| `neutral` | 😐 | `winking` | 😉 |
| `happy` | 🙂 | `cool` | 😎 |
| `laughing` | 😆 | `relaxed` | 😌 |
| `funny` | 😄 | `delicious` | 😋 |
| `sad` | 🙁 | `kissy` | 😘 |
| `angry` | 😠 | `confident` | 😏 |
| `crying` | 😢 | `sleepy` | 😴 |
| `loving` | 😍 | `silly` | 😜 |
| `embarrassed` | 😳 | `confused` | 🙄 |
| `surprised` | 😮 | | |
| `shocked` | 😱 | | |
| `thinking` | 🤔 | | |

未识别的字符串设备端 fallback 到 `FONT_AWESOME_NEUTRAL`（`oled_display.cc:396`），所以**写错也不会崩**。

### 10.2 设备端做了什么

`xiaozhi-esp32/main/application.cc:699-703`：

```cpp
auto emotion = cJSON_GetObjectItem(root, "emotion");
if (cJSON_IsString(emotion)) {
    Schedule([display, emotion_str = std::string(emotion->valuestring)]() {
        display->SetEmotion(emotion_str.c_str());
    });
}
```

`SetEmotion`（`oled_display.cc:387-398`）改的是 `emotion_label_`——LVGL 上**一个独立的 widget**，跟 `status_label_` / `chat_message_label_` 互不干扰：

```cpp
void OledDisplay::SetEmotion(const char* emotion) {
    const char* utf8 = font_awesome_get_utf8(emotion);
    if (utf8 != nullptr) {
        lv_label_set_text(emotion_label_, utf8);
    } else {
        lv_label_set_text(emotion_label_, FONT_AWESOME_NEUTRAL);
    }
}
```

### 10.3 不影响什么

`emotion` **只改屏幕上的表情图标**，**不**触发：

- ❌ 状态机切换（`Idle` / `Listening` / `Speaking` 互转）
- ❌ 状态文字（`STANDBY` / `LISTENING` / `SPEAKING`）
- ❌ AEC 开/关
- ❌ 解码队列
- ❌ Mic 上行

`Speaking` 状态由 `tts.start` 触发，由 `tts.stop` 退出。`emotion` 完全独立。

### 10.4 当前实现为什么是 hardcoded

`direct_device.go:470`：

```go
_ = session.writeJSON(map[string]any{"type": "llm", "emotion": "neutral"})
```

`neutral` 是写死的。要让 LLM 真正决定表情，需要：

1. 改 `GoLLMPrompt`（`config.go:100`），让 LLM 输出 JSON `{"answer":"...","emotion":"happy"}`
2. `playAssistantText` 加 `emotion string` 参数
3. `direct_device.go:470` 改成传 `emotion` 变量

---

## 11. 状态机影响总结

```
[server]                            [ESP32 device]

llm.emotion=neutral ───text───→   SetEmotion()            只改表情图标

tts.state=start    ───text───→   SetDeviceState(Speaking)
                                 ├ SetStatus(SPEAKING)
                                 ├ EnableVoiceProcessing(false)
                                 ├ ResetDecoder()        ← 清队列
                                 └ Mic 上行：停

tts.state=stop     ───text───→   SetDeviceState(Idle/Listening)
                                 ├ SetStatus(STANDBY/LISTENING)
                                 ├ EnableVoiceProcessing(true)
                                 ├ Mic 上行：开
                                 └ audio_playback_queue_：不动 ← 注意

（audio_playback_queue_ 还要 ~300ms 才排空）
```

### 关键时序错位

```
t=0ms      设备收到 tts.start
           状态：Speaking，AEC off，prebuffer 5 包灌入

t=900ms    服务端发完最后一个 Opus 包
           紧跟着发 tts.stop
           设备收到 tts.stop
           ↓
           状态：Speaking → Listening     ← 状态机立即切
           屏幕：LISTENING                 ← UI 立即变
           AEC：on                         ← 立即开
           Mic 上行：恢复                  ← 立即开始推
           ↑
           但 audio_playback_queue_ 里还压着
           5 包 × 60ms = 300ms 音频没放 ↓
           ↓
t=1200ms   扬声器把 buffer 里最后几个 60ms 包放完
           真正安静
```

---

## 12. 与老 Python 服务端的关系

老服务端是 `xiaozhi-esp32-server`（第三方项目，不在本仓库），通过 HTTP bridge 跟当前 Go admin 通信：

```
Go admin  ──HTTP/POST /bridge/speak {device_id, text}──→  xiaozhi-esp32-server
       ←──JSON {ok,status,device_id}──────────────────
```

WebSocket 协议**完全一致**（同一份 ESP32 固件能连两端）：

| 维度 | Python | Go（本仓库） |
|---|---|---|
| 上游 format | 历史上是 wav | 强制 opus（`direct_tts.go:50-52`） |
| Ogg 解析 | 标准库 | 手写 `extractOpusPackets` |
| 重编码 | decode→encode 60ms | 同款（`direct_ogg.go:253` 注释："matching the Python server's approach"） |
| WebSocket 协议 | 同款 JSON+Binary | 同款 |

`direct_ogg.go:256` 的注释直接说明本实现对齐 Python 端。

---

## 13. 参考实现：Mac 客户端

`xiaoli-mac/` 是同协议的桌面端参考实现（开发/调试用），**不影响 ESP32 设备逻辑**。它连到同一 WebSocket 路径 `/xiaozhi/v1/`，但状态机、显示、解码都用本机 Go 代码实现：

| 关注点 | Mac 端位置 |
|---|---|
| WebSocket 接收 | `xiaoli-mac/protocol/transport/transport.go:175-191` |
| 状态机 | `xiaoli-mac/state/state.go`（`idle` / `listening` / `speaking`） |
| TTS 状态处理 | `xiaoli-mac/protocol/dispatcher.go:62-80` |
| 表情显示 | `xiaoli-mac/display/fynegui/display.go:158-162` |
| Emoji 表 | `xiaoli-mac/display/fynegui/display.go:353+` |
| 裸 Opus 帧落盘（`record_tts`） | `xiaoli-mac/app/app.go:267-279` |

文档主体不再展开 Mac 端。

---

## 14. 关键文件位置

### 服务端（Go）

| 关注点 | 位置 |
|---|---|
| 服务端发 TTS 事件 | `server/internal/admin/direct_device.go:465-475` |
| TTS 流控+prebuffer | `server/internal/admin/direct_device.go:512-540` |
| TTS 常量 | `server/internal/admin/direct_device.go:20-23` |
| Ogg 解析+重编码 | `server/internal/admin/direct_ogg.go:181-313` |
| Ogg 写回（罕见路径） | `server/internal/admin/direct_ogg.go:16-92` |
| 上游 TTS 调用 | `server/internal/admin/direct_tts.go:46-90` |
| WebSocket 协议层 | `server/internal/admin/direct_ws.go:16-122` |
| 设备 hello 响应 | `server/internal/admin/direct_device.go:295-307` |

### 设备端（ESP32）

| 关注点 | 位置 |
|---|---|
| WebSocket 接收 | `xiaozhi-esp32/main/protocols/websocket_protocol.cc:112-249` |
| 设备 hello 发送 | `xiaozhi-esp32/main/protocols/websocket_protocol.cc:203-225` |
| 设备→服务端事件发送 | `xiaozhi-esp32/main/protocols/protocol.cc:42-79` |
| 设备处理 llm 事件 | `xiaozhi-esp32/main/application.cc:695-704` |
| 设备处理 tts 事件 | `xiaozhi-esp32/main/application.cc:635-676` |
| 设备状态机副作用 | `xiaozhi-esp32/main/application.cc:1114-1123` |
| 设备解码循环 | `xiaozhi-esp32/main/audio/audio_service.cc:335-394` |
| 设备常量 | `xiaozhi-esp32/main/audio/audio_service.h:39-43` |
| 表情映射表 | `xiaozhi-esp32/main/display/lvgl_display/emoji_collection.cc:54-74` |
| 表情渲染 | `xiaozhi-esp32/main/display/oled_display.cc:387-398` |

---

## 14. 外部参考仓库

本仓库的设备固件和管理端都是从上游开源项目**派生**出来的，重要参考依据：

| 角色 | 上游仓库 | 说明 |
|---|---|---|
| **板子**（ESP32 固件） | <https://github.com/78/xiaozhi-esp32> | 本仓库 `xiaozhi-esp32/` 是其 fork/定制，Ogg Opus 解析、60ms 帧解码、状态机、LVGL 表情等逻辑均来自上游。本文档引用的 `xiaozhi-esp32/main/...` 路径需在上游仓库对照查阅。 |
| **管理端**（Python 服务） | <https://github.com/xinnan-tech/xiaozhi-esp32-server> | 老服务端，本仓库的 Go admin 通过 HTTP bridge 调用其 WebSocket/MCP/TTS 能力（详见 [§11](#11-与老-python-服务端的关系)）。WebSocket 协议与设备端是**同套**的——上游文档里描述的协议字段、事件类型、设备行为，都可作为本仓库实现的参考。 |

> 涉及协议字段或设备行为有疑问时，**优先翻上游**。本仓库只在派生时做了少量修改（见 `xiaozhi-esp32/main/boards/` 下各板级目录的 commit 历史和本仓库的 patch 文件）。
