# Xiaoli Admin Remote Device Control

## Goal

Add a protected web admin console to the existing xiaoli server so the owner can remotely call functions exposed by the connected ESP32 board, such as taking a photo, querying status, and any other MCP tools the device advertises.

## Authentication

The admin console uses a temporary fixed Token while Logto callback setup is blocked. The Token is provided through the `ADMIN_ACCESS_TOKEN` Fly secret, not hardcoded source values. Logto OIDC support remains in the server module as a fallback path when `ADMIN_ACCESS_TOKEN` is not configured.

Required configuration:

- `ADMIN_SESSION_SECRET`
- `ADMIN_ACCESS_TOKEN`

The server creates its own `xiaoli_admin_session` cookie after the correct Token is submitted at `/admin/login`. The submitted Token is compared server-side and is never stored in the browser.

## Architecture

The current repository wraps the upstream `xiaozhi-esp32-server` image. The upstream WebSocket process already owns the device MCP bridge, so the admin feature is added inside the same Python process instead of as a separate sidecar.

Build-time patches will:

- copy `xiaoli_admin.py` into `/opt/xiaozhi-esp32-server`;
- patch `app.py` to start `XiaoliAdminServer` alongside the existing WebSocket and OTA services;
- patch `WebSocketServer` to track online `ConnectionHandler` instances by `device_id`;
- expose `/admin`, `/admin/login`, `/admin/callback`, `/admin/logout`, and `/admin/api/*` through Nginx.

## Admin API

The first version provides:

- `GET /admin/api/me`: current user claims.
- `GET /admin/api/devices`: connected device list and MCP readiness.
- `GET /admin/api/tools?device_id=...`: tools from the device MCP client.
- `POST /admin/api/call`: direct MCP tool call with `device_id`, `tool`, `arguments`, and optional `timeout`.

The API never asks the LLM to decide whether to call a tool. It calls the device MCP bridge directly.

## UI

The `/admin` page is a focused operations dashboard:

- device selector/status;
- quick actions for `self.get_device_status` and `self.camera.take_photo`;
- a generic MCP tool caller based on the live tool list;
- result panel showing JSON/text returned by the tool;
- confirmation before camera or microphone-like tools run.

If a requested tool is not exposed by the connected device, the UI shows that the device does not advertise the capability instead of assuming firmware changes.

## Audit And Safety

Every admin tool call logs timestamp, user identifier, device id, tool name, arguments, success/failure, and result summary to the server log. The admin Token and session cookies are never logged.

Camera and microphone-related tool names require an explicit confirmation in the UI.

## Verification

Verification covers:

- unit tests for environment rendering and Nginx admin proxy routes;
- Python compile checks for the entrypoint and admin server code;
- local static checks where possible;
- deploy verification that `/admin` shows the Token login flow and that unauthenticated `/admin/api/*` requests are rejected.
