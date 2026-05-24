import os
import re
import signal
import subprocess
import sys
import time
from pathlib import Path


PROJECT_DIR = Path("/opt/xiaozhi-esp32-server")
DATA_DIR = PROJECT_DIR / "data"
CONFIG_PATH = DATA_DIR / ".config.yaml"
CONFIG_TEMPLATE_PATH = Path("/fly/config.template.yaml")
NGINX_TEMPLATE_PATH = Path("/fly/nginx.conf")
NGINX_CONFIG_PATH = Path("/etc/nginx/nginx.conf")
INSECURE_DEFAULT_AUTH_KEY = "change-me-before-public-use"
DEVICE_ID_PATTERN = re.compile(r"^[A-Za-z0-9:._-]+$")


DEFAULTS = {
    "PUBLIC_BASE_URL": "https://xiaoli-server.fly.dev",
    "PUBLIC_WS_URL": "",
    "PUBLIC_VISION_URL": "",
    "SERVER_AUTH_KEY": INSECURE_DEFAULT_AUTH_KEY,
    "ENABLE_SERVER_AUTH": "true",
    "ALLOWED_DEVICE_ID": "",
    "ALLOWED_DEVICE_IDS": "",
    "SERVER_AUTH_ALLOWED_DEVICE_IDS": "",
    "DEVICE_NAME": "小李",
    "ASR_MODULE": "SiliconFlowASR",
    "LLM_MODULE": "SiliconFlowLLM",
    "VLLM_MODULE": "SiliconFlowVLLM",
    "TTS_MODULE": "SiliconFlowTTS",
    "OPENAI_API_KEY": "",
    "OPENAI_ASR_BASE_URL": "https://api.openai.com/v1/audio/transcriptions",
    "OPENAI_ASR_MODEL": "gpt-4o-mini-transcribe",
    "OPENAI_LLM_MODEL": "gpt-4o-mini",
    "OPENROUTER_API_KEY": "",
    "OPENROUTER_LLM_MODEL": "openrouter/free",
    "OPENROUTER_VLLM_MODEL": "openrouter/free",
    "GROQ_API_KEY": "",
    "SILICONFLOW_API_KEY": "",
    "SILICONFLOW_LLM_MODEL": "Qwen/Qwen3-8B",
    "SILICONFLOW_VLLM_MODEL": "Qwen/Qwen3-VL-8B-Instruct",
    "SILICONFLOW_ASR_MODEL": "FunAudioLLM/SenseVoiceSmall",
    "SILICONFLOW_TTS_MODEL": "FunAudioLLM/CosyVoice2-0.5B",
    "SILICONFLOW_TTS_VOICE": "FunAudioLLM/CosyVoice2-0.5B:anna",
    "SILICONFLOW_TTS_RESPONSE_FORMAT": "wav",
    "ZHIPU_API_KEY": "",
    "DASHSCOPE_API_KEY": "",
    "QWEATHER_API_KEY": "",
    "WEB_SEARCH_API_KEY": "",
    "XIAOLI_ADMIN_ENABLED": "false",
    "XIAOLI_ADMIN_PORT": "8004",
    "ADMIN_PUBLIC_BASE_URL": "",
    "LOGTO_ENDPOINT": "",
    "LOGTO_APP_ID": "",
    "ADMIN_ALLOWED_USERS": "",
}


def bool_env(value):
    return str(value).strip().lower() in {"1", "true", "yes", "on"}


def parse_device_ids(value):
    devices = []
    seen = set()
    for item in value.split(","):
        device_id = item.strip()
        if not device_id:
            continue
        if not DEVICE_ID_PATTERN.fullmatch(device_id):
            raise RuntimeError(f"Invalid device id in allowlist: {device_id}")
        if device_id not in seen:
            devices.append(device_id)
            seen.add(device_id)
    return devices


def yaml_list(items, indent=6):
    if not items:
        return "[]"
    spaces = " " * indent
    return "\n" + "\n".join(f'{spaces}- "{item}"' for item in items)


def nginx_device_map(devices):
    return "".join(f'        "{device_id}" 1;\n' for device_id in devices)


def websocket_auth_guard(enabled):
    if not enabled:
        return ""
    return (
        "            if ($http_authorization = \"\") {\n"
        "                return 401;\n"
        "            }\n\n"
    )


def admin_nginx_routes(enabled, port):
    if not enabled:
        return (
            "        location = /admin {\n"
            "            return 404;\n"
            "        }\n\n"
            "        location /admin/ {\n"
            "            return 404;\n"
            "        }\n"
        )
    return (
        "        location = /admin {\n"
        f"            proxy_pass http://127.0.0.1:{port};\n"
        "            proxy_set_header Host $host;\n"
        "            proxy_set_header X-Real-IP $remote_addr;\n"
        "            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n"
        "            proxy_set_header X-Forwarded-Proto $scheme;\n"
        "            proxy_set_header X-Forwarded-Host $host;\n"
        "        }\n\n"
        "        location /admin/ {\n"
        f"            proxy_pass http://127.0.0.1:{port};\n"
        "            proxy_set_header Host $host;\n"
        "            proxy_set_header X-Real-IP $remote_addr;\n"
        "            proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n"
        "            proxy_set_header X-Forwarded-Proto $scheme;\n"
        "            proxy_set_header X-Forwarded-Host $host;\n"
        "        }\n"
    )


def build_values():
    values = {key: os.environ.get(key, default) for key, default in DEFAULTS.items()}
    base_url = values["PUBLIC_BASE_URL"].rstrip("/")
    if not values["PUBLIC_WS_URL"]:
        values["PUBLIC_WS_URL"] = base_url.replace("https://", "wss://").replace("http://", "ws://") + "/xiaozhi/v1/"
    if not values["PUBLIC_VISION_URL"]:
        values["PUBLIC_VISION_URL"] = base_url + "/mcp/vision/explain"
    auth_enabled = bool_env(values["ENABLE_SERVER_AUTH"])
    values["ENABLE_SERVER_AUTH"] = "true" if auth_enabled else "false"
    if auth_enabled:
        auth_key = values["SERVER_AUTH_KEY"]
        if auth_key == INSECURE_DEFAULT_AUTH_KEY or len(auth_key) < 32:
            raise RuntimeError("SERVER_AUTH_KEY must be set to a strong random value when ENABLE_SERVER_AUTH=true")

    allowed_device_ids = values["ALLOWED_DEVICE_IDS"] or values["ALLOWED_DEVICE_ID"]
    edge_allowed_devices = parse_device_ids(allowed_device_ids)
    server_auth_allowed_devices = parse_device_ids(values["SERVER_AUTH_ALLOWED_DEVICE_IDS"])
    values["NGINX_ALLOWED_DEVICE_MAP"] = nginx_device_map(edge_allowed_devices)
    values["EDGE_ALLOWED_DEVICE_IDS"] = ", ".join(edge_allowed_devices) if edge_allowed_devices else "(none)"
    values["SERVER_AUTH_ALLOWED_DEVICES"] = yaml_list(server_auth_allowed_devices)
    values["WEBSOCKET_AUTH_GUARD"] = websocket_auth_guard(auth_enabled)
    admin_enabled = bool_env(values["XIAOLI_ADMIN_ENABLED"])
    values["XIAOLI_ADMIN_ENABLED"] = "true" if admin_enabled else "false"
    values["ADMIN_NGINX_ROUTES"] = admin_nginx_routes(admin_enabled, values["XIAOLI_ADMIN_PORT"])
    if not values["ADMIN_PUBLIC_BASE_URL"]:
        values["ADMIN_PUBLIC_BASE_URL"] = base_url
    return values


def render_template(path, values):
    content = path.read_text(encoding="utf-8")
    for key, value in values.items():
        content = content.replace(f"__{key}__", value)
    return content


def render_config():
    values = build_values()
    config_content = render_template(CONFIG_TEMPLATE_PATH, values)
    nginx_content = render_template(NGINX_TEMPLATE_PATH, values)

    DATA_DIR.mkdir(parents=True, exist_ok=True)
    NGINX_CONFIG_PATH.parent.mkdir(parents=True, exist_ok=True)
    CONFIG_PATH.write_text(config_content, encoding="utf-8")
    NGINX_CONFIG_PATH.write_text(nginx_content, encoding="utf-8")
    print(f"Rendered config to {CONFIG_PATH}", flush=True)
    print(f"Rendered nginx config to {NGINX_CONFIG_PATH}", flush=True)
    print(f"Public WebSocket: {values['PUBLIC_WS_URL']}", flush=True)
    print(f"Public vision URL: {values['PUBLIC_VISION_URL']}", flush=True)
    print(f"Edge allowed devices: {values['EDGE_ALLOWED_DEVICE_IDS']}", flush=True)
    print(f"Xiaoli admin enabled: {values['XIAOLI_ADMIN_ENABLED']}", flush=True)


def start_processes():
    app = subprocess.Popen(["python", "app.py"], cwd=PROJECT_DIR)
    nginx = subprocess.Popen(["nginx", "-g", "daemon off;"])
    children = [app, nginx]

    def stop(signum, frame):
        print(f"Received signal {signum}, stopping services", flush=True)
        for child in children:
            if child.poll() is None:
                child.terminate()
        deadline = time.time() + 10
        while time.time() < deadline and any(child.poll() is None for child in children):
            time.sleep(0.2)
        for child in children:
            if child.poll() is None:
                child.kill()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)

    while True:
        for child in children:
            code = child.poll()
            if code is not None:
                stop(signal.SIGTERM, None)
                return code
        time.sleep(1)


def main():
    render_config()
    return start_processes()


if __name__ == "__main__":
    sys.exit(main())
