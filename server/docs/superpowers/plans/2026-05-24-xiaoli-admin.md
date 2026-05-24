# Xiaoli Admin Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a Token-protected admin dashboard that directly calls MCP tools on connected ESP32 devices.

**Architecture:** Patch the upstream Python service at image build time so the admin HTTP server runs in the same event loop as the existing WebSocket server. Track online `ConnectionHandler` instances in `WebSocketServer`, and have admin APIs call the existing `device_mcp.call_mcp_tool` helper. Nginx proxies `/admin` to the new internal admin port, and the temporary login flow exchanges `ADMIN_ACCESS_TOKEN` for an HMAC-signed admin session cookie.

**Tech Stack:** Python 3, aiohttp, HMAC-signed cookies, Nginx, Fly secrets.

---

### Task 1: Admin Server Module

**Files:**
- Create: `fly/xiaoli_admin.py`

- [ ] **Step 1: Implement signed cookie helpers**

Create helpers that HMAC-sign JSON payloads using `ADMIN_SESSION_SECRET`, enforce max age, and never log cookie contents.

- [ ] **Step 2: Implement temporary Token login handlers**

Implement `/admin/login` and `/admin/logout`. Login renders a Token form, compares the submitted Token with `ADMIN_ACCESS_TOKEN`, and stores a signed admin session cookie. Keep the existing Logto OIDC code path available only when no `ADMIN_ACCESS_TOKEN` is configured.

- [ ] **Step 3: Implement admin API handlers**

Implement `/admin/api/me`, `/admin/api/devices`, `/admin/api/tools`, and `/admin/api/call`. The call handler validates JSON input, finds the active device handler, calls `call_mcp_tool`, and logs an audit event.

- [ ] **Step 4: Implement dashboard HTML**

Serve a compact dashboard at `/admin` with device status, quick buttons for status/photo, a generic tool caller, and confirmation for camera/audio-like tools.

### Task 2: Upstream Patch Script

**Files:**
- Create: `fly/patch_admin.py`
- Modify: `Dockerfile`

- [ ] **Step 1: Patch `app.py`**

Insert `from xiaoli_admin import XiaoliAdminServer`, create `admin_server = XiaoliAdminServer(config, ws_server)`, start it as `admin_task`, and include it in shutdown cancellation/wait logic.

- [ ] **Step 2: Patch `core/websocket_server.py`**

Add an `active_connections` registry guarded by `asyncio.Lock`. Register the handler after authentication and unregister only if the same handler is still current for that `device_id`.

- [ ] **Step 3: Wire Docker build**

Copy `fly/xiaoli_admin.py` and `fly/patch_admin.py` into `/opt/xiaozhi-esp32-server`, then run the patch script during image build.

### Task 3: Nginx And Entrypoint Configuration

**Files:**
- Modify: `fly/entrypoint.py`
- Modify: `fly/nginx.conf`
- Modify: `fly.toml`
- Modify: `.env.example`
- Modify: `README.md`

- [ ] **Step 1: Add admin defaults**

Add `XIAOLI_ADMIN_ENABLED`, `XIAOLI_ADMIN_PORT`, and `ADMIN_PUBLIC_BASE_URL` to rendered defaults. Keep `ADMIN_ACCESS_TOKEN` and `ADMIN_SESSION_SECRET` out of committed files.

- [ ] **Step 2: Render Nginx admin routes**

When admin is enabled, proxy `/admin` and `/admin/` to `127.0.0.1:${XIAOLI_ADMIN_PORT}`. When disabled, return 404 for `/admin`.

- [ ] **Step 3: Document secrets**

Document required Fly secrets: `ADMIN_ACCESS_TOKEN` and `ADMIN_SESSION_SECRET`.

### Task 4: Tests And Verification

**Files:**
- Modify: `tests/test_entrypoint_security.py`
- Create or modify: tests for template rendering if needed.

- [ ] **Step 1: Test admin route rendering**

Add tests for enabled admin proxy and disabled admin 404 rendering.

- [ ] **Step 2: Compile Python files**

Run `PYTHONDONTWRITEBYTECODE=1 python3 -m py_compile fly/entrypoint.py fly/xiaoli_admin.py tests/test_entrypoint_security.py`.

- [ ] **Step 3: Run unit tests**

Run `PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests -p 'test_*.py' -v`.

- [ ] **Step 4: Build check**

Run `flyctl deploy --build-only` or `flyctl deploy` after secrets are set, depending on whether deployment is requested.

### Task 5: Fly Secret Setup

**Files:**
- No source files.

- [ ] **Step 1: Generate temporary admin secrets**

Generate a random `ADMIN_ACCESS_TOKEN` and a random `ADMIN_SESSION_SECRET`.

- [ ] **Step 2: Set Fly secrets without printing secret values**

Set `ADMIN_ACCESS_TOKEN` and `ADMIN_SESSION_SECRET` via `flyctl secrets set`.

- [ ] **Step 3: Post-deploy smoke checks**

Verify `/admin` redirects to `/admin/login`, `/admin/login` renders the Token form, `/admin/api/devices` rejects unauthenticated requests, and existing `/health`, OTA, and WebSocket routes still work.
