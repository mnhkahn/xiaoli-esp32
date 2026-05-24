import asyncio
import base64
import hashlib
import hmac
import json
import os
import re
import secrets
import time
from html import escape
from typing import Any
from urllib.parse import urlencode, urljoin

from aiohttp import BasicAuth, ClientSession, web
from config.logger import setup_logging
from core.providers.tools.device_mcp.mcp_handler import call_mcp_tool


TAG = __name__
SESSION_COOKIE = "xiaoli_admin_session"
STATE_COOKIE = "xiaoli_admin_state"
IMAGE_KEYS = ("image", "imageurl", "image_url", "photo", "picture", "thumbnail", "base64", "url")
TEXT_KEYS = ("description", "explain", "analysis", "text", "message", "answer", "caption", "summary")


def _b64encode(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def _b64decode(raw: str) -> bytes:
    padding = "=" * (-len(raw) % 4)
    return base64.urlsafe_b64decode((raw + padding).encode("ascii"))


def _csv(value: str) -> list[str]:
    return [item.strip() for item in value.split(",") if item.strip()]


def _env_bool(name: str, default: str = "false") -> bool:
    return os.environ.get(name, default).strip().lower() in {"1", "true", "yes", "on"}


def _json_response(payload: dict[str, Any], status: int = 200) -> web.Response:
    return web.json_response(payload, status=status, dumps=lambda obj: json.dumps(obj, ensure_ascii=False))


class XiaoliAdminError(RuntimeError):
    pass


class XiaoliAdminServer:
    def __init__(self, config: dict[str, Any], ws_server: Any):
        self.config = config
        self.ws_server = ws_server
        self.logger = setup_logging()
        self.enabled = _env_bool("XIAOLI_ADMIN_ENABLED")
        self.host = os.environ.get("XIAOLI_ADMIN_HOST", "0.0.0.0")
        self.port = int(os.environ.get("XIAOLI_ADMIN_PORT", "8004"))
        self.public_base_url = os.environ.get("ADMIN_PUBLIC_BASE_URL") or os.environ.get(
            "PUBLIC_BASE_URL", "https://xiaoli-server.fly.dev"
        )
        self.public_base_url = self.public_base_url.rstrip("/")
        self.redirect_uri = os.environ.get(
            "LOGTO_REDIRECT_URI", f"{self.public_base_url}/admin/callback"
        )
        self.post_logout_redirect_uri = os.environ.get(
            "LOGTO_POST_LOGOUT_REDIRECT_URI", f"{self.public_base_url}/admin"
        )
        self.logto_endpoint = os.environ.get("LOGTO_ENDPOINT", "").rstrip("/") + "/"
        self.logto_app_id = os.environ.get("LOGTO_APP_ID", "")
        self.logto_app_secret = os.environ.get("LOGTO_APP_SECRET", "")
        self.admin_access_token = os.environ.get("ADMIN_ACCESS_TOKEN", "")
        self.auth_mode = "token" if self.admin_access_token else "logto"
        self.session_secret = os.environ.get("ADMIN_SESSION_SECRET", "")
        self.allowed_users = set(_csv(os.environ.get("ADMIN_ALLOWED_USERS", "")))
        self.allow_all_users = "*" in self.allowed_users
        self.session_max_age = int(os.environ.get("ADMIN_SESSION_MAX_AGE_SECONDS", "604800"))
        self.mcp_ready_wait_seconds = float(os.environ.get("ADMIN_MCP_READY_WAIT_SECONDS", "5"))
        self.oidc_config: dict[str, Any] | None = None

    async def start(self):
        if not self.enabled:
            self.logger.bind(tag=TAG).info("Xiaoli admin server is disabled")
            return
        self._validate_config()
        if self.auth_mode == "logto":
            await self._load_oidc_config()

        app = web.Application()
        app.add_routes(
            [
                web.get("/admin", self.handle_index),
                web.get("/admin/", self.handle_index),
                web.get("/admin/login", self.handle_login),
                web.post("/admin/login", self.handle_login),
                web.get("/admin/callback", self.handle_callback),
                web.get("/admin/logout", self.handle_logout),
                web.get("/admin/api/me", self.handle_me),
                web.get("/admin/api/devices", self.handle_devices),
                web.get("/admin/api/tools", self.handle_tools),
                web.post("/admin/api/call", self.handle_call),
            ]
        )

        runner = web.AppRunner(app)
        await runner.setup()
        site = web.TCPSite(runner, self.host, self.port)
        await site.start()
        self.logger.bind(tag=TAG).info(f"Xiaoli admin server listening on {self.host}:{self.port}")
        while True:
            await asyncio.sleep(3600)

    def _validate_config(self):
        if not self.session_secret:
            raise RuntimeError("Missing admin configuration: ADMIN_SESSION_SECRET")
        if len(self.session_secret) < 32:
            raise RuntimeError("ADMIN_SESSION_SECRET must be at least 32 characters")
        if self.auth_mode == "token":
            if len(self.admin_access_token) < 24:
                raise RuntimeError("ADMIN_ACCESS_TOKEN must be at least 24 characters")
            return

        missing = []
        for name, value in [
            ("LOGTO_ENDPOINT", self.logto_endpoint.strip("/")),
            ("LOGTO_APP_ID", self.logto_app_id),
            ("LOGTO_APP_SECRET", self.logto_app_secret),
        ]:
            if not value:
                missing.append(name)
        if missing:
            raise RuntimeError("Missing admin configuration: " + ", ".join(missing))
        if not self.allowed_users:
            raise RuntimeError("ADMIN_ALLOWED_USERS must include at least one Logto sub/email/username/name")

    async def _load_oidc_config(self):
        discovery_url = urljoin(self.logto_endpoint, "oidc/.well-known/openid-configuration")
        async with ClientSession() as session:
            async with session.get(discovery_url, timeout=10) as response:
                if response.status >= 400:
                    text = await response.text()
                    raise RuntimeError(f"Load Logto OIDC config failed: {response.status} {text[:200]}")
                self.oidc_config = await response.json()

    def _sign(self, payload: dict[str, Any]) -> str:
        encoded = _b64encode(json.dumps(payload, separators=(",", ":"), ensure_ascii=False).encode("utf-8"))
        signature = hmac.new(self.session_secret.encode("utf-8"), encoded.encode("ascii"), hashlib.sha256).digest()
        return f"{encoded}.{_b64encode(signature)}"

    def _verify(self, token: str, max_age: int | None = None) -> dict[str, Any]:
        try:
            encoded, signature = token.split(".", 1)
            expected = hmac.new(
                self.session_secret.encode("utf-8"), encoded.encode("ascii"), hashlib.sha256
            ).digest()
            if not hmac.compare_digest(_b64decode(signature), expected):
                raise XiaoliAdminError("bad signature")
            payload = json.loads(_b64decode(encoded).decode("utf-8"))
            now = int(time.time())
            if payload.get("exp") and int(payload["exp"]) < now:
                raise XiaoliAdminError("expired")
            if max_age is not None and int(payload.get("iat", 0)) + max_age < now:
                raise XiaoliAdminError("too old")
            return payload
        except Exception as exc:
            raise XiaoliAdminError("invalid signed cookie") from exc

    def _set_signed_cookie(
        self,
        response: web.StreamResponse,
        name: str,
        payload: dict[str, Any],
        max_age: int,
    ):
        response.set_cookie(
            name,
            self._sign(payload),
            max_age=max_age,
            httponly=True,
            secure=True,
            samesite="Lax",
            path="/admin",
        )

    def _clear_cookie(self, response: web.StreamResponse, name: str):
        response.del_cookie(name, path="/admin")

    def _user_allowed(self, user: dict[str, Any]) -> bool:
        if self.auth_mode == "token":
            return user.get("sub") == "token-admin"
        if self.allow_all_users:
            return True
        candidates = {
            str(user.get("sub", "")),
            str(user.get("email", "")),
            str(user.get("username", "")),
            str(user.get("name", "")),
        }
        return bool(candidates & self.allowed_users)

    def _get_user(self, request: web.Request) -> dict[str, Any] | None:
        token = request.cookies.get(SESSION_COOKIE)
        if not token:
            return None
        try:
            session = self._verify(token)
        except XiaoliAdminError:
            return None
        user = session.get("user")
        if self.auth_mode == "token" and session.get("auth_mode") != "token":
            return None
        if not isinstance(user, dict) or not self._user_allowed(user):
            return None
        return user

    def _require_user(self, request: web.Request) -> dict[str, Any]:
        user = self._get_user(request)
        if not user:
            raise web.HTTPUnauthorized(text="unauthorized")
        return user

    def _sanitize_return_to(self, return_to: str) -> str:
        if not return_to.startswith("/admin"):
            return "/admin"
        return return_to

    def _login_redirect(self, return_to: str = "/admin") -> web.Response:
        return_to = self._sanitize_return_to(return_to)
        if self.auth_mode == "token":
            return web.HTTPFound("/admin/login?" + urlencode({"return_to": return_to}))

        state = secrets.token_urlsafe(24)
        nonce = secrets.token_urlsafe(24)
        code_verifier = secrets.token_urlsafe(48)
        code_challenge = _b64encode(hashlib.sha256(code_verifier.encode("ascii")).digest())
        query = {
            "client_id": self.logto_app_id,
            "redirect_uri": self.redirect_uri,
            "response_type": "code",
            "scope": "openid profile email",
            "state": state,
            "nonce": nonce,
            "code_challenge": code_challenge,
            "code_challenge_method": "S256",
        }
        assert self.oidc_config is not None
        location = self.oidc_config["authorization_endpoint"] + "?" + urlencode(query)
        response = web.HTTPFound(location)
        self._set_signed_cookie(
            response,
            STATE_COOKIE,
            {
                "state": state,
                "nonce": nonce,
                "code_verifier": code_verifier,
                "return_to": return_to,
                "iat": int(time.time()),
            },
            max_age=600,
        )
        return response

    async def handle_login(self, request: web.Request) -> web.Response:
        return_to = self._sanitize_return_to(request.query.get("return_to", "/admin"))
        if self.auth_mode == "token":
            if request.method == "GET":
                return web.Response(text=self._token_login_html(return_to), content_type="text/html")
            data = await request.post()
            return self._create_token_login_response(str(data.get("token", "")), return_to)
        return self._login_redirect(return_to)

    def _create_token_login_response(self, provided_token: str, return_to: str = "/admin") -> web.Response:
        return_to = self._sanitize_return_to(return_to)
        if not hmac.compare_digest(provided_token, self.admin_access_token):
            raise web.HTTPUnauthorized(
                text=self._token_login_html(return_to, "Token 不正确"),
                content_type="text/html",
            )

        now = int(time.time())
        response = web.HTTPFound(return_to)
        self._clear_cookie(response, STATE_COOKIE)
        self._set_signed_cookie(
            response,
            SESSION_COOKIE,
            {
                "auth_mode": "token",
                "user": {"sub": "token-admin", "name": "Admin Token"},
                "iat": now,
                "exp": now + self.session_max_age,
            },
            max_age=self.session_max_age,
        )
        return response

    def _token_login_html(self, return_to: str = "/admin", error: str = "") -> str:
        safe_action = escape("/admin/login?" + urlencode({"return_to": self._sanitize_return_to(return_to)}))
        error_html = f'<p class="error">{escape(error)}</p>' if error else ""
        return f"""<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>小李后台登录</title>
  <style>
    :root {{
      color-scheme: light;
      --bg: #f6f7f9;
      --panel: #ffffff;
      --text: #17202a;
      --muted: #667085;
      --line: #d9dee7;
      --accent: #0f766e;
      --danger: #b42318;
    }}
    * {{ box-sizing: border-box; }}
    body {{ margin: 0; min-height: 100vh; display: grid; place-items: center; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: var(--bg); color: var(--text); }}
    main {{ width: min(420px, calc(100vw - 32px)); background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 20px; }}
    h1 {{ margin: 0 0 16px; font-size: 20px; }}
    label {{ display: block; margin-bottom: 8px; color: var(--muted); font-size: 13px; }}
    input, button {{ width: 100%; font: inherit; border-radius: 6px; padding: 10px; }}
    input {{ border: 1px solid var(--line); margin-bottom: 12px; }}
    button {{ border: 1px solid var(--accent); background: var(--accent); color: #fff; cursor: pointer; }}
    .error {{ margin: 0 0 12px; color: var(--danger); font-size: 13px; }}
  </style>
</head>
<body>
  <main>
    <h1>小李设备后台</h1>
    {error_html}
    <form method="post" action="{safe_action}">
      <label for="token">后台 Token</label>
      <input id="token" name="token" type="password" autocomplete="current-password" autofocus>
      <button type="submit">登录</button>
    </form>
  </main>
</body>
</html>"""

    async def handle_callback(self, request: web.Request) -> web.Response:
        if self.auth_mode == "token":
            raise web.HTTPNotFound(text="not found")
        code = request.query.get("code", "")
        state = request.query.get("state", "")
        if not code or not state:
            raise web.HTTPBadRequest(text="missing code or state")
        state_cookie = request.cookies.get(STATE_COOKIE, "")
        try:
            expected_state = self._verify(state_cookie, max_age=600)
        except XiaoliAdminError:
            raise web.HTTPBadRequest(text="invalid login state")
        if not hmac.compare_digest(str(expected_state.get("state", "")), state):
            raise web.HTTPBadRequest(text="state mismatch")

        assert self.oidc_config is not None
        token_endpoint = self.oidc_config["token_endpoint"]
        userinfo_endpoint = self.oidc_config["userinfo_endpoint"]
        token_data = {
            "grant_type": "authorization_code",
            "code": code,
            "redirect_uri": self.redirect_uri,
            "code_verifier": expected_state.get("code_verifier", ""),
        }

        async with ClientSession() as session:
            async with session.post(
                token_endpoint,
                data=token_data,
                auth=BasicAuth(self.logto_app_id, self.logto_app_secret),
                timeout=15,
            ) as response:
                if response.status >= 400:
                    token_payload = await self._exchange_token_with_secret_post(
                        session, token_endpoint, token_data
                    )
                else:
                    token_payload = await response.json()

            access_token = token_payload.get("access_token", "")
            if not access_token:
                raise web.HTTPBadRequest(text="missing access token")
            async with session.get(
                userinfo_endpoint,
                headers={"Authorization": f"Bearer {access_token}"},
                timeout=15,
            ) as response:
                if response.status >= 400:
                    text = await response.text()
                    raise web.HTTPBadRequest(text=f"load userinfo failed: {text[:200]}")
                userinfo = await response.json()

        user = {
            "sub": userinfo.get("sub"),
            "email": userinfo.get("email"),
            "name": userinfo.get("name"),
            "username": userinfo.get("username"),
        }
        if not self._user_allowed(user):
            raise web.HTTPForbidden(text="user is not allowed")

        now = int(time.time())
        response = web.HTTPFound(str(expected_state.get("return_to") or "/admin"))
        self._clear_cookie(response, STATE_COOKIE)
        self._set_signed_cookie(
            response,
            SESSION_COOKIE,
            {"user": user, "iat": now, "exp": now + self.session_max_age},
            max_age=self.session_max_age,
        )
        return response

    async def _exchange_token_with_secret_post(
        self,
        session: ClientSession,
        token_endpoint: str,
        token_data: dict[str, Any],
    ) -> dict[str, Any]:
        fallback_data = {
            **token_data,
            "client_id": self.logto_app_id,
            "client_secret": self.logto_app_secret,
        }
        async with session.post(token_endpoint, data=fallback_data, timeout=15) as response:
            if response.status >= 400:
                text = await response.text()
                raise web.HTTPBadRequest(text=f"token exchange failed: {text[:200]}")
        return await response.json()

    async def handle_logout(self, request: web.Request) -> web.Response:
        location = "/admin/login" if self.auth_mode == "token" else self.post_logout_redirect_uri
        if self.auth_mode == "logto":
            query = {
                "client_id": self.logto_app_id,
                "post_logout_redirect_uri": self.post_logout_redirect_uri,
            }
            assert self.oidc_config is not None
            end_session_endpoint = self.oidc_config.get("end_session_endpoint")
            if end_session_endpoint:
                location = end_session_endpoint + "?" + urlencode(query)
        response = web.HTTPFound(location)
        self._clear_cookie(response, SESSION_COOKIE)
        self._clear_cookie(response, STATE_COOKIE)
        return response

    async def handle_index(self, request: web.Request) -> web.Response:
        user = self._get_user(request)
        if not user:
            return self._login_redirect(request.path_qs if request.path_qs.startswith("/admin") else "/admin")
        return web.Response(text=self._dashboard_html(user), content_type="text/html")

    async def handle_me(self, request: web.Request) -> web.Response:
        user = self._require_user(request)
        return _json_response({"user": user})

    async def handle_devices(self, request: web.Request) -> web.Response:
        self._require_user(request)
        devices = await self.ws_server.list_admin_devices()
        return _json_response({"devices": devices})

    async def handle_tools(self, request: web.Request) -> web.Response:
        self._require_user(request)
        handler = await self._get_handler_for_request(request)
        mcp_client = getattr(handler, "mcp_client", None)
        if not mcp_client or not await self._wait_for_mcp_ready(mcp_client):
            return _json_response({"tools": [], "ready": False})
        return _json_response({"tools": mcp_client.get_available_tools(), "ready": True})

    async def handle_call(self, request: web.Request) -> web.Response:
        user = self._require_user(request)
        try:
            body = await request.json()
        except Exception:
            raise web.HTTPBadRequest(text="invalid json")

        device_id = str(body.get("device_id") or "")
        tool_name = str(body.get("tool") or "")
        arguments = body.get("arguments") or {}
        timeout = int(body.get("timeout") or self.config.get("tool_call_timeout", 30) or 30)
        timeout = max(1, min(timeout, 120))
        if not device_id or not tool_name:
            raise web.HTTPBadRequest(text="device_id and tool are required")
        if not isinstance(arguments, dict):
            raise web.HTTPBadRequest(text="arguments must be an object")

        handler = await self.ws_server.get_admin_connection(device_id)
        if not handler:
            raise web.HTTPNotFound(text="device is not online")
        mcp_client = getattr(handler, "mcp_client", None)
        if not mcp_client or not await self._wait_for_mcp_ready(mcp_client):
            raise web.HTTPConflict(text="device MCP is not ready")

        started = time.time()
        user_id = user.get("email") or user.get("username") or user.get("name") or user.get("sub")
        try:
            result = await call_mcp_tool(handler, mcp_client, tool_name, arguments, timeout=timeout)
            elapsed_ms = int((time.time() - started) * 1000)
            self._audit(user_id, device_id, tool_name, arguments, True, result, elapsed_ms)
            parsed = self._try_json(result)
            preview = self._build_result_preview(parsed)
            return _json_response(
                {"ok": True, "result": parsed, "raw": result, "preview": preview, "elapsed_ms": elapsed_ms}
            )
        except Exception as exc:
            elapsed_ms = int((time.time() - started) * 1000)
            self._audit(user_id, device_id, tool_name, arguments, False, str(exc), elapsed_ms)
            return _json_response({"ok": False, "error": str(exc), "elapsed_ms": elapsed_ms}, status=500)

    async def _get_handler_for_request(self, request: web.Request):
        device_id = request.query.get("device_id", "")
        if device_id:
            handler = await self.ws_server.get_admin_connection(device_id)
            if not handler:
                raise web.HTTPNotFound(text="device is not online")
            return handler
        devices = await self.ws_server.list_admin_devices()
        if len(devices) != 1:
            raise web.HTTPBadRequest(text="device_id is required")
        handler = await self.ws_server.get_admin_connection(devices[0]["device_id"])
        if not handler:
            raise web.HTTPNotFound(text="device is not online")
        return handler

    async def _wait_for_mcp_ready(
        self,
        mcp_client: Any,
        timeout: float | None = None,
        interval: float = 0.2,
    ) -> bool:
        deadline = time.monotonic() + (self.mcp_ready_wait_seconds if timeout is None else timeout)
        while True:
            try:
                if await mcp_client.is_ready():
                    return True
            except Exception:
                return False
            if time.monotonic() >= deadline:
                return False
            await asyncio.sleep(interval)

    def _audit(
        self,
        user_id: str,
        device_id: str,
        tool_name: str,
        arguments: dict[str, Any],
        ok: bool,
        result: Any,
        elapsed_ms: int,
    ):
        args_text = json.dumps(arguments, ensure_ascii=False)
        result_text = str(result)
        if len(args_text) > 500:
            args_text = args_text[:500] + "..."
        if len(result_text) > 500:
            result_text = result_text[:500] + "..."
        self.logger.bind(tag=TAG).info(
            "admin_tool_call user={} device={} tool={} ok={} elapsed_ms={} args={} result={}",
            user_id,
            device_id,
            tool_name,
            ok,
            elapsed_ms,
            args_text,
            result_text,
        )

    def _try_json(self, value: Any) -> Any:
        if not isinstance(value, str):
            return value
        try:
            return json.loads(value)
        except Exception:
            return value

    def _build_result_preview(self, value: Any) -> dict[str, Any]:
        images: list[str] = []
        texts: list[str] = []
        seen_images: set[str] = set()
        seen_texts: set[str] = set()

        def add_image(src: str):
            if src and src not in seen_images and len(images) < 8:
                images.append(src)
                seen_images.add(src)

        def add_text(text: str):
            text = text.strip()
            if text and text not in seen_texts and len(texts) < 8:
                texts.append(text)
                seen_texts.add(text)

        def key_name(key: Any) -> str:
            return re.sub(r"[^a-z0-9]", "", str(key).lower())

        def image_src(key: Any, raw: str) -> str:
            raw = raw.strip()
            normalized = key_name(key)
            image_like_key = any(item in normalized for item in IMAGE_KEYS)
            if raw.startswith("data:image/"):
                return raw
            if raw.startswith(("http://", "https://")):
                clean = raw.split("?", 1)[0].lower()
                if image_like_key or clean.endswith((".jpg", ".jpeg", ".png", ".webp", ".gif")):
                    return raw
            if image_like_key and re.fullmatch(r"[A-Za-z0-9+/=\s]+", raw):
                return "data:image/jpeg;base64," + re.sub(r"\s+", "", raw)
            return ""

        def walk(node: Any, key: Any = ""):
            if isinstance(node, dict):
                for child_key, child_value in node.items():
                    walk(child_value, child_key)
                return
            if isinstance(node, list):
                for item in node:
                    walk(item, key)
                return
            if not isinstance(node, str):
                return

            src = image_src(key, node)
            if src:
                add_image(src)
                return
            normalized = key_name(key)
            if any(item in normalized for item in TEXT_KEYS):
                add_text(node)

        walk(value)
        return {"images": images, "text": "\n\n".join(texts)}

    def _dashboard_html(self, user: dict[str, Any]) -> str:
        user_label = escape(str(user.get("email") or user.get("name") or user.get("sub") or "admin"))
        return f"""<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>小李设备后台</title>
  <style>
    :root {{
      color-scheme: light;
      --bg: #f6f7f9;
      --panel: #ffffff;
      --text: #17202a;
      --muted: #667085;
      --line: #d9dee7;
      --accent: #0f766e;
      --danger: #b42318;
      --ok: #067647;
    }}
    * {{ box-sizing: border-box; }}
    body {{ margin: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: var(--bg); color: var(--text); }}
    header {{ display: flex; align-items: center; justify-content: space-between; padding: 14px 20px; border-bottom: 1px solid var(--line); background: var(--panel); }}
    h1 {{ margin: 0; font-size: 18px; font-weight: 650; }}
    main {{ max-width: 1180px; margin: 0 auto; padding: 18px; display: grid; gap: 16px; grid-template-columns: 300px 1fr; }}
    section {{ background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 14px; }}
    h2 {{ margin: 0 0 12px; font-size: 15px; }}
    button, select, input, textarea {{ font: inherit; }}
    button {{ border: 1px solid var(--line); background: #fff; border-radius: 6px; padding: 8px 10px; cursor: pointer; }}
    button.primary {{ background: var(--accent); color: #fff; border-color: var(--accent); }}
    button.danger {{ color: var(--danger); }}
    button:disabled {{ opacity: .5; cursor: not-allowed; }}
    select, input, textarea {{ width: 100%; border: 1px solid var(--line); border-radius: 6px; padding: 8px; background: #fff; }}
    textarea {{ min-height: 150px; resize: vertical; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 13px; }}
    pre {{ margin: 0; white-space: pre-wrap; word-break: break-word; background: #101828; color: #e6edf3; border-radius: 6px; padding: 12px; min-height: 240px; overflow: auto; }}
    .photo-preview {{ display: grid; gap: 10px; }}
    .photo-preview:empty, .interpretation:empty {{ display: none; }}
    .photo-preview img {{ width: 100%; max-height: 520px; object-fit: contain; border: 1px solid var(--line); border-radius: 6px; background: #f2f4f7; }}
    .interpretation {{ border: 1px solid var(--line); border-radius: 6px; padding: 10px; white-space: pre-wrap; line-height: 1.5; }}
    .muted {{ color: var(--muted); font-size: 13px; }}
    .row {{ display: flex; gap: 8px; align-items: center; }}
    .stack {{ display: grid; gap: 10px; }}
    .device {{ border: 1px solid var(--line); border-radius: 6px; padding: 10px; margin-bottom: 8px; cursor: pointer; }}
    .device.active {{ border-color: var(--accent); box-shadow: 0 0 0 1px var(--accent) inset; }}
    .pill {{ display: inline-block; border-radius: 999px; padding: 2px 8px; font-size: 12px; background: #eef2f6; color: var(--muted); }}
    .pill.ok {{ background: #dcfae6; color: var(--ok); }}
    .grid2 {{ display: grid; gap: 16px; grid-template-columns: 1fr 1fr; }}
    @media (max-width: 820px) {{ main, .grid2 {{ grid-template-columns: 1fr; }} }}
  </style>
</head>
<body>
  <header>
    <h1>小李设备后台</h1>
    <div class="row"><span class="muted">{user_label}</span><a href="/admin/logout">退出</a></div>
  </header>
  <main>
    <section>
      <h2>在线设备</h2>
      <div id="devices" class="stack"></div>
      <button id="refreshDevices">刷新</button>
    </section>
    <div class="stack">
      <section>
        <h2>快捷操作</h2>
        <div class="row">
          <button class="primary" data-quick="self.get_device_status">查状态</button>
          <button class="primary" data-quick="self.camera.take_photo">拍照</button>
          <button data-record>录音</button>
        </div>
        <p class="muted">快捷操作直接调用设备 MCP 工具，不经过模型判断。</p>
      </section>
      <section class="grid2">
        <div class="stack">
          <h2>工具调用</h2>
          <label class="muted">工具</label>
          <select id="toolSelect"></select>
          <label class="muted">参数 JSON</label>
          <textarea id="argsInput">{{}}</textarea>
          <button id="callTool" class="primary">调用工具</button>
        </div>
        <div class="stack">
          <h2>结果</h2>
          <div id="photoPreview" class="photo-preview"></div>
          <div id="interpretation" class="interpretation"></div>
          <pre id="result">等待操作...</pre>
        </div>
      </section>
    </div>
  </main>
  <script>
    let selectedDevice = "";
    let tools = [];
    const risky = /camera|photo|mic|microphone|audio|record|录音|拍照|摄像/i;

    function show(value) {{
      const text = typeof value === "string" ? value : JSON.stringify(value, null, 2);
      document.getElementById("photoPreview").innerHTML = "";
      document.getElementById("interpretation").textContent = "";
      document.getElementById("result").textContent = text;
    }}

    function renderResult(data) {{
      const preview = (data && data.preview) || {{}};
      const images = Array.isArray(preview.images) ? preview.images : [];
      const photoBox = document.getElementById("photoPreview");
      const textBox = document.getElementById("interpretation");
      photoBox.innerHTML = "";
      textBox.textContent = "";
      for (const src of images) {{
        if (!src) continue;
        const img = document.createElement("img");
        img.src = src;
        img.alt = "设备拍摄图片";
        photoBox.appendChild(img);
      }}
      if (preview.text) textBox.textContent = preview.text;
      document.getElementById("result").textContent = JSON.stringify(data, null, 2);
    }}

    async function api(path, options = {{}}) {{
      const response = await fetch(path, {{
        credentials: "same-origin",
        headers: {{ "Content-Type": "application/json", ...(options.headers || {{}}) }},
        ...options
      }});
      const text = await response.text();
      let data = text;
      try {{ data = JSON.parse(text); }} catch (_) {{}}
      if (!response.ok) throw new Error(typeof data === "string" ? data : JSON.stringify(data));
      return data;
    }}

    async function loadDevices() {{
      const data = await api("/admin/api/devices");
      const box = document.getElementById("devices");
      box.innerHTML = "";
      if (!data.devices.length) {{
        box.innerHTML = '<p class="muted">当前没有在线设备</p>';
        selectedDevice = "";
        renderTools([]);
        return;
      }}
      data.devices.forEach((device, index) => {{
        if (!selectedDevice && index === 0) selectedDevice = device.device_id;
        const el = document.createElement("div");
        el.className = "device" + (device.device_id === selectedDevice ? " active" : "");
        el.innerHTML = `<strong>${{device.device_id}}</strong><br><span class="pill ${{device.mcp_ready ? "ok" : ""}}">${{device.mcp_ready ? "MCP ready" : "MCP not ready"}}</span> <span class="pill">${{device.client_ip || ""}}</span>`;
        el.onclick = async () => {{ selectedDevice = device.device_id; await loadDevices(); await loadTools(); }};
        box.appendChild(el);
      }});
      await loadTools();
    }}

    function renderTools(items) {{
      tools = items;
      const select = document.getElementById("toolSelect");
      select.innerHTML = "";
      for (const item of items) {{
        const fn = item.function || {{}};
        const opt = document.createElement("option");
        opt.value = fn.name;
        opt.textContent = fn.name;
        select.appendChild(opt);
      }}
      updateArgsFromTool();
    }}

    async function loadTools() {{
      if (!selectedDevice) return renderTools([]);
      const data = await api(`/admin/api/tools?device_id=${{encodeURIComponent(selectedDevice)}}`);
      renderTools(data.tools || []);
    }}

    async function ensureToolsReady() {{
      if (!selectedDevice) throw new Error("没有选择在线设备");
      if (!tools.length) {{
        show("正在获取设备工具列表...");
        await loadTools();
      }}
      return tools.length > 0;
    }}

    function updateArgsFromTool() {{
      const toolName = document.getElementById("toolSelect").value;
      const tool = tools.find(t => (t.function || {{}}).name === toolName);
      const props = (((tool || {{}}).function || {{}}).parameters || {{}}).properties || {{}};
      const args = {{}};
      for (const [key, schema] of Object.entries(props)) {{
        if (key === "question") args[key] = "请描述这张照片里的内容";
        else if (schema.type === "integer" || schema.type === "number") args[key] = 0;
        else if (schema.type === "boolean") args[key] = false;
        else args[key] = "";
      }}
      document.getElementById("argsInput").value = JSON.stringify(args, null, 2);
    }}

    function sanitizeToolName(name) {{
      return name.replace(/[^a-zA-Z0-9_\\-\\u4e00-\\u9fff]/g, "_");
    }}

    function resolveToolName(name) {{
      const names = new Set([name, sanitizeToolName(name)]);
      for (const item of tools) {{
        const fn = item.function || {{}};
        if (names.has(fn.name)) return fn.name;
      }}
      return "";
    }}

    function findRecordTool() {{
      const candidates = [/record/i, /audio/i, /microphone/i, /mic/i, /录音/];
      for (const item of tools) {{
        const fn = item.function || {{}};
        const text = `${{fn.name}} ${{fn.description || ""}}`;
        if (candidates.some(pattern => pattern.test(text))) return fn.name;
      }}
      return "";
    }}

    async function callTool(toolName, args) {{
      if (!selectedDevice) throw new Error("没有选择在线设备");
      if (risky.test(toolName) && !confirm(`确认调用 ${{toolName}}？`)) return;
      show("调用中...");
      const data = await api("/admin/api/call", {{
        method: "POST",
        body: JSON.stringify({{ device_id: selectedDevice, tool: toolName, arguments: args || {{}} }})
      }});
      renderResult(data);
    }}

    document.getElementById("refreshDevices").onclick = loadDevices;
    document.getElementById("toolSelect").onchange = updateArgsFromTool;
    document.getElementById("callTool").onclick = async () => {{
      try {{
        const args = JSON.parse(document.getElementById("argsInput").value || "{{}}");
        await callTool(document.getElementById("toolSelect").value, args);
      }} catch (error) {{ show(String(error)); }}
    }};
    document.querySelectorAll("[data-quick]").forEach(button => {{
      button.onclick = async () => {{
        try {{ await ensureToolsReady(); }} catch (error) {{ return show(String(error)); }}
        const tool = resolveToolName(button.dataset.quick);
        if (!tool) return show(`设备未暴露工具：${{button.dataset.quick}}`);
        const args = button.dataset.quick === "self.camera.take_photo" ? {{ question: "请描述这张照片里的内容" }} : {{}};
        try {{ await callTool(tool, args); }} catch (error) {{ show(String(error)); }}
      }};
    }});
    document.querySelector("[data-record]").onclick = async () => {{
      try {{
        await ensureToolsReady();
        const tool = findRecordTool();
        if (!tool) return show("设备当前工具列表里没有录音相关能力");
        await callTool(tool, {{}});
      }} catch (error) {{ show(String(error)); }}
    }};
    loadDevices().catch(error => show(String(error)));
  </script>
</body>
</html>"""
