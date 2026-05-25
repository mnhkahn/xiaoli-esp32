import importlib.util
import os
import unittest
from pathlib import Path
from unittest.mock import patch


ENTRYPOINT_PATH = Path(__file__).resolve().parents[1] / "fly" / "entrypoint.py"
DOCKERFILE_PATH = Path(__file__).resolve().parents[1] / "Dockerfile"
SPEC = importlib.util.spec_from_file_location("entrypoint", ENTRYPOINT_PATH)
entrypoint = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(entrypoint)


class EntryPointSecurityTest(unittest.TestCase):
    def test_auth_enabled_rejects_default_auth_key(self):
        with patch.dict(os.environ, {"ENABLE_SERVER_AUTH": "true"}, clear=True):
            with self.assertRaisesRegex(RuntimeError, "SERVER_AUTH_KEY"):
                entrypoint.build_values()

    def test_edge_device_allowlist_does_not_bypass_upstream_auth(self):
        env = {
            "ENABLE_SERVER_AUTH": "true",
            "SERVER_AUTH_KEY": "0123456789abcdef0123456789abcdef",
            "ALLOWED_DEVICE_IDS": "28:84:85:8c:ef:f4, aa:bb:cc:dd:ee:ff",
        }
        with patch.dict(os.environ, env, clear=True):
            values = entrypoint.build_values()

        self.assertEqual(values["SERVER_AUTH_ALLOWED_DEVICES"], "[]")
        self.assertIn('"28:84:85:8c:ef:f4" 1;', values["NGINX_ALLOWED_DEVICE_MAP"])
        self.assertIn('"aa:bb:cc:dd:ee:ff" 1;', values["NGINX_ALLOWED_DEVICE_MAP"])

    def test_websocket_auth_guard_is_rendered_when_auth_enabled(self):
        env = {
            "ENABLE_SERVER_AUTH": "true",
            "SERVER_AUTH_KEY": "0123456789abcdef0123456789abcdef",
            "ALLOWED_DEVICE_IDS": "28:84:85:8c:ef:f4",
        }
        with patch.dict(os.environ, env, clear=True):
            values = entrypoint.build_values()

        self.assertIn("$http_authorization", values["WEBSOCKET_AUTH_GUARD"])
        self.assertIn("return 401", values["WEBSOCKET_AUTH_GUARD"])

    def test_ota_route_does_not_require_edge_device_header(self):
        env = {
            "ENABLE_SERVER_AUTH": "true",
            "SERVER_AUTH_KEY": "0123456789abcdef0123456789abcdef",
            "ALLOWED_DEVICE_IDS": "28:84:85:8c:ef:f4",
        }
        with patch.dict(os.environ, env, clear=True):
            values = entrypoint.build_values()

        nginx = entrypoint.render_template(ENTRYPOINT_PATH.parent / "nginx.conf", values)
        ota_section = nginx.split("location /xiaozhi/ota/ {", 1)[1].split("location /mcp/vision/ {", 1)[0]
        websocket_section = nginx.split("location /xiaozhi/v1/ {", 1)[1].split("location /xiaozhi/ota/ {", 1)[0]
        vision_section = nginx.split("location /mcp/vision/ {", 1)[1].split("location / {", 1)[0]

        self.assertNotIn("$xiaoli_allowed_device", ota_section)
        self.assertIn("$xiaoli_allowed_device", websocket_section)
        self.assertIn("$xiaoli_allowed_device", vision_section)

    def test_rendered_templates_contain_no_security_placeholders(self):
        env = {
            "ENABLE_SERVER_AUTH": "true",
            "SERVER_AUTH_KEY": "0123456789abcdef0123456789abcdef",
            "ALLOWED_DEVICE_IDS": "28:84:85:8c:ef:f4",
            "XIAOLI_ADMIN_ENABLED": "true",
        }
        with patch.dict(os.environ, env, clear=True):
            values = entrypoint.build_values()

        config = entrypoint.render_template(ENTRYPOINT_PATH.parent / "config.template.yaml", values)
        nginx = entrypoint.render_template(ENTRYPOINT_PATH.parent / "nginx.conf", values)

        self.assertIn("enabled: true", config)
        self.assertIn("allowed_devices: []", config)
        self.assertIn('"28:84:85:8c:ef:f4" 1;', nginx)
        self.assertIn("return 401", nginx)
        self.assertIn("proxy_pass http://127.0.0.1:8004", nginx)
        self.assertNotIn("__SERVER_AUTH_ALLOWED_DEVICES__", config)
        self.assertNotIn("__NGINX_ALLOWED_DEVICE_MAP__", nginx)
        self.assertNotIn("__ADMIN_NGINX_ROUTES__", nginx)

    def test_default_vllm_uses_siliconflow_vision_model(self):
        env = {
            "ENABLE_SERVER_AUTH": "true",
            "SERVER_AUTH_KEY": "0123456789abcdef0123456789abcdef",
        }
        with patch.dict(os.environ, env, clear=True):
            values = entrypoint.build_values()

        config = entrypoint.render_template(ENTRYPOINT_PATH.parent / "config.template.yaml", values)

        self.assertEqual(values["VLLM_MODULE"], "SiliconFlowVLLM")
        self.assertIn("VLLM: SiliconFlowVLLM", config)
        self.assertIn("model_name: Qwen/Qwen3-VL-8B-Instruct", config)
        self.assertIn("url: https://api.siliconflow.cn/v1/", config)

    def test_admin_disabled_renders_not_found_routes(self):
        env = {
            "ENABLE_SERVER_AUTH": "true",
            "SERVER_AUTH_KEY": "0123456789abcdef0123456789abcdef",
            "XIAOLI_ADMIN_ENABLED": "false",
        }
        with patch.dict(os.environ, env, clear=True):
            values = entrypoint.build_values()

        self.assertIn("location = /admin", values["ADMIN_NGINX_ROUTES"])
        self.assertIn("return 404", values["ADMIN_NGINX_ROUTES"])

    def test_admin_enabled_renders_proxy_routes(self):
        env = {
            "ENABLE_SERVER_AUTH": "true",
            "SERVER_AUTH_KEY": "0123456789abcdef0123456789abcdef",
            "XIAOLI_ADMIN_ENABLED": "true",
            "XIAOLI_ADMIN_PORT": "8123",
        }
        with patch.dict(os.environ, env, clear=True):
            values = entrypoint.build_values()

        self.assertIn("location = /admin", values["ADMIN_NGINX_ROUTES"])
        self.assertIn("location /admin/", values["ADMIN_NGINX_ROUTES"])
        self.assertIn("proxy_pass http://127.0.0.1:8123", values["ADMIN_NGINX_ROUTES"])
        self.assertIn("proxy_set_header Upgrade $http_upgrade", values["ADMIN_NGINX_ROUTES"])

    def test_go_admin_process_starts_when_admin_enabled(self):
        env = {
            "ENABLE_SERVER_AUTH": "true",
            "SERVER_AUTH_KEY": "0123456789abcdef0123456789abcdef",
            "XIAOLI_ADMIN_ENABLED": "true",
        }
        with patch.dict(os.environ, env, clear=True):
            values = entrypoint.build_values()

        commands = entrypoint.process_commands(values)

        self.assertIn(["python", "app.py"], commands)
        self.assertIn(["/usr/local/bin/xiaoli-admin"], commands)
        self.assertIn(["nginx", "-g", "daemon off;"], commands)

    def test_go_admin_process_is_skipped_when_admin_disabled(self):
        env = {
            "ENABLE_SERVER_AUTH": "true",
            "SERVER_AUTH_KEY": "0123456789abcdef0123456789abcdef",
            "XIAOLI_ADMIN_ENABLED": "false",
        }
        with patch.dict(os.environ, env, clear=True):
            values = entrypoint.build_values()

        commands = entrypoint.process_commands(values)

        self.assertIn(["python", "app.py"], commands)
        self.assertNotIn(["/usr/local/bin/xiaoli-admin"], commands)
        self.assertIn(["nginx", "-g", "daemon off;"], commands)

    def test_connection_timeout_allows_idle_admin_connection(self):
        env = {
            "ENABLE_SERVER_AUTH": "true",
            "SERVER_AUTH_KEY": "0123456789abcdef0123456789abcdef",
        }
        with patch.dict(os.environ, env, clear=True):
            values = entrypoint.build_values()

        config = entrypoint.render_template(ENTRYPOINT_PATH.parent / "config.template.yaml", values)

        self.assertIn("close_connection_no_voice_time: 3600", config)

    def test_vision_route_uses_admin_proxy_when_admin_enabled(self):
        env = {
            "ENABLE_SERVER_AUTH": "true",
            "SERVER_AUTH_KEY": "0123456789abcdef0123456789abcdef",
            "XIAOLI_ADMIN_ENABLED": "true",
            "XIAOLI_ADMIN_PORT": "8123",
        }
        with patch.dict(os.environ, env, clear=True):
            values = entrypoint.build_values()

        nginx = entrypoint.render_template(ENTRYPOINT_PATH.parent / "nginx.conf", values)
        vision_section = nginx.split("location /mcp/vision/ {", 1)[1].split("location / {", 1)[0]

        self.assertIn("proxy_pass http://127.0.0.1:8123", vision_section)

    def test_vision_route_uses_http_server_when_admin_disabled(self):
        env = {
            "ENABLE_SERVER_AUTH": "true",
            "SERVER_AUTH_KEY": "0123456789abcdef0123456789abcdef",
            "XIAOLI_ADMIN_ENABLED": "false",
        }
        with patch.dict(os.environ, env, clear=True):
            values = entrypoint.build_values()

        nginx = entrypoint.render_template(ENTRYPOINT_PATH.parent / "nginx.conf", values)
        vision_section = nginx.split("location /mcp/vision/ {", 1)[1].split("location / {", 1)[0]

        self.assertIn("proxy_pass http://127.0.0.1:8003", vision_section)

    def test_dockerfile_installs_and_applies_langsmith_patch(self):
        dockerfile = DOCKERFILE_PATH.read_text(encoding="utf-8")

        self.assertIn("pip install --no-cache-dir langsmith", dockerfile)
        self.assertIn("COPY fly/xiaoli_langsmith.py", dockerfile)
        self.assertIn("COPY fly/patch_langsmith.py", dockerfile)
        self.assertIn("RUN python /fly/patch_langsmith.py", dockerfile)

    def test_dockerfile_builds_go_admin_and_copies_bridge(self):
        dockerfile = DOCKERFILE_PATH.read_text(encoding="utf-8")

        self.assertIn("FROM golang:1.23", dockerfile)
        self.assertIn("go build -o /out/xiaoli-admin ./cmd/xiaoli-admin", dockerfile)
        self.assertIn("COPY --from=go-build /out/xiaoli-admin /usr/local/bin/xiaoli-admin", dockerfile)
        self.assertIn("COPY fly/xiaoli_bridge.py /opt/xiaozhi-esp32-server/xiaoli_bridge.py", dockerfile)
        self.assertNotIn("COPY fly/xiaoli_admin.py", dockerfile)


if __name__ == "__main__":
    unittest.main()
