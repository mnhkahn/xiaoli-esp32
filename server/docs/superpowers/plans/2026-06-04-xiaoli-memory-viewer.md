# Xiaoli Memory Viewer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a read-only `/admin/memory` page and JSON APIs for inspecting Redis-backed Eino memory by device.

**Architecture:** Reuse the existing Go admin server, Logto session middleware, and inline HTML style. Add a small Redis-memory read API around the existing `redisMemory` client and render a standalone memory viewer page that fetches those APIs.

**Tech Stack:** Go `net/http`, `github.com/redis/go-redis/v9`, Eino `schema.Message`, existing Go tests.

---

### Task 1: Tests For Memory Page And API Surface

**Files:**
- Modify: `server/internal/admin/admin_test.go`

- [ ] Add tests proving `/admin` links to `/admin/memory`, `/admin/memory` renders a standalone page, and unauthenticated memory APIs return 401.
- [ ] Run `go test ./internal/admin -run 'TestDashboardLinksToMemoryViewer|TestMemoryPageRendersStandaloneViewer|TestMemoryAPIsRequireAuth'` and verify the tests fail because the routes and page do not exist yet.

### Task 2: Tests For Redis Memory Listing And Detail

**Files:**
- Modify: `server/internal/admin/admin_test.go`

- [ ] Add an in-memory fake Redis reader and tests for memory list/detail behavior without requiring a real Redis server.
- [ ] Cover newest-first default ordering, oldest-first ordering, TTL fields, disabled Redis, and malformed JSON.
- [ ] Run the focused tests and verify they fail because the memory API implementation does not exist yet.

### Task 3: Implement Memory Reader, Routes, And Page

**Files:**
- Modify: `server/internal/admin/direct_ai.go`
- Modify: `server/internal/admin/server.go`

- [ ] Add a narrow memory reader interface implemented by `redisMemory`.
- [ ] Store that reader on `AdminServer`.
- [ ] Add `/admin/memory`, `/admin/api/memory`, and `/admin/api/memory/detail` routes behind existing admin auth.
- [ ] Add `memoryHTML` and a `/admin` link.
- [ ] Run focused tests until green.

### Task 4: Full Verification

**Files:**
- Verify: `server/internal/admin`

- [ ] Run `go test ./internal/admin`.
- [ ] Run `go test ./...`.
- [ ] Review `git diff` for accidental unrelated changes.
