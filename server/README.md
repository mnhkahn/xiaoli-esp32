# Xiaoli Server on Fly.io

This directory deploys a server-only xiaozhi backend to Fly.io.

The container runs the upstream `xiaozhi-esp32-server` image and adds an Nginx
front proxy so Fly can expose both services on one HTTPS hostname:

- `https://<app>.fly.dev/xiaozhi/ota/` -> internal HTTP/OTA service on `8003`
- `wss://<app>.fly.dev/xiaozhi/v1/` -> internal WebSocket service on `8000`
- `https://<app>.fly.dev/mcp/vision/explain` -> internal vision service on `8003`
- `https://<app>.fly.dev/admin` -> Token-protected admin console on `8004`

## First Deploy

Install and log in:

```bash
brew install flyctl
fly auth login
```

Create the app once:

```bash
cd server
fly apps create xiaoli-server
```

If you change the app name, update `app` and `PUBLIC_BASE_URL` in `fly.toml`.

Set required model secrets. For the current defaults, the minimum useful set is:

```bash
fly secrets set OPENROUTER_API_KEY=your_openrouter_key
fly secrets set SILICONFLOW_API_KEY=your_siliconflow_key
fly secrets set SERVER_AUTH_KEY=$(openssl rand -hex 32)
fly secrets set ADMIN_ACCESS_TOKEN=$(openssl rand -base64 32)
fly secrets set ADMIN_SESSION_SECRET=$(openssl rand -hex 32)
```

Server authentication is enabled by default. OTA is left reachable so devices
can check updates and fetch the current WebSocket URL. `ALLOWED_DEVICE_IDS` is
used by the Nginx edge gate for WebSocket and vision requests; it is not
rendered into the upstream `server.auth.allowed_devices` list, because that
upstream list bypasses token verification. Keep `SERVER_AUTH_KEY` as a Fly
secret and rotate it if it is ever exposed.

The admin console currently uses a fixed temporary Token. Keep
`ADMIN_ACCESS_TOKEN` as a long random Fly secret, open `/admin`, and paste the
Token into the login form. Rotate it if it is ever exposed.

Deploy:

```bash
fly deploy
```

Check health:

```bash
curl https://xiaoli-server.fly.dev/health
curl https://xiaoli-server.fly.dev/xiaozhi/ota/
```

## Firmware OTA URL

After the Fly app is deployed, rebuild the firmware with:

```text
CONFIG_OTA_URL="https://xiaoli-server.fly.dev/xiaozhi/ota/"
```

Then flash the board.

## Configuration

Runtime config is rendered from `fly/config.template.yaml` into:

```text
/opt/xiaozhi-esp32-server/data/.config.yaml
```

Important environment variables:

- `PUBLIC_BASE_URL`: public HTTPS base URL, for example `https://xiaoli-server.fly.dev`
- `ENABLE_SERVER_AUTH`: default `true`; OTA issues WebSocket tokens and the WebSocket server verifies them
- `ALLOWED_DEVICE_IDS`: comma-separated device IDs allowed through Nginx for WebSocket and vision requests
- `SERVER_AUTH_KEY`: signing key for WebSocket and vision tokens; set as a secret
- `SERVER_AUTH_ALLOWED_DEVICE_IDS`: optional upstream bypass list; normally leave empty so token verification is not bypassed
- `XIAOLI_ADMIN_ENABLED`: enables the admin console when `true`
- `XIAOLI_ADMIN_PORT`: internal admin port; default `8004`
- `ADMIN_ACCESS_TOKEN`: temporary login Token for `/admin`; set as a secret
- `ADMIN_SESSION_SECRET`: signing key for admin cookies; set as a secret
- `ASR_MODULE`: default `SiliconFlowASR`
- `LLM_MODULE`: default `SiliconFlowLLM`
- `VLLM_MODULE`: default `SiliconFlowVLLM`
- `TTS_MODULE`: default `SiliconFlowTTS`
- `OPENROUTER_API_KEY`: used by `OpenRouterLLM` and `OpenRouterVLLM`
- `OPENROUTER_LLM_MODEL`: default `openrouter/free`
- `OPENROUTER_VLLM_MODEL`: default `openrouter/free`
- `SILICONFLOW_API_KEY`: used by `SiliconFlowASR`, `SiliconFlowLLM`, `SiliconFlowVLLM`, and `SiliconFlowTTS`
- `SILICONFLOW_LLM_MODEL`: default `Qwen/Qwen3-8B`
- `SILICONFLOW_VLLM_MODEL`: default `Qwen/Qwen3-VL-8B-Instruct`
- `SILICONFLOW_ASR_MODEL`: default `FunAudioLLM/SenseVoiceSmall`
- `SILICONFLOW_TTS_MODEL`: default `FunAudioLLM/CosyVoice2-0.5B`
- `SILICONFLOW_TTS_VOICE`: default `FunAudioLLM/CosyVoice2-0.5B:anna`
- `GROQ_API_KEY`: used only if switching back to `GroqASR`
- `OPENAI_API_KEY`: used only if switching back to `OpenaiASR`
- `ZHIPU_API_KEY`: used only if switching back to `ChatGLMLLM` / `ChatGLMVLLM`
- `DASHSCOPE_API_KEY`: used if switching to `AliLLM` / `QwenVLVLLM`

Use `fly secrets set` for keys. Do not commit real keys.
