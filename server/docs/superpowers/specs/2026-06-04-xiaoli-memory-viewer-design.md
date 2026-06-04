# Xiaoli Memory Viewer Design

## Goal

Add a protected read-only memory viewer to the Go admin server so the owner can
inspect Redis-backed Eino conversation memory by device.

## Current Memory Shape

Memory is stored as Redis string values, not hashes or sets.

- Key: `XIAOLI_REDIS_KEY_PREFIX + deviceID`
- Default key example: `xiaoli:cp:28:84:85:8c:ef:f4`
- Value: JSON array of Eino `schema.Message` objects
- TTL: `XIAOLI_MEMORY_TTL_HOURS`, default 24 hours
- Retention: last 40 messages

The current save path appends messages in chronological order, oldest to newest:
`history + user + assistant`.

## UI

The existing `/admin` dashboard keeps device-control workflows. It adds a simple
link to `/admin/memory`.

`/admin/memory` is a separate Logto-protected page with:

- left panel: online devices plus Redis keys matching the configured prefix;
- center panel: message timeline;
- right panel: selected message details and raw JSON;
- controls: refresh, device/key filter, sort newest-first or oldest-first.

The default display order is newest first because memory inspection is usually
used to debug the latest interaction. The page can switch to oldest first to
replay the conversation in model input order.

## API

Add read-only admin API endpoints:

- `GET /admin/api/memory`: list memory keys and online device metadata.
- `GET /admin/api/memory/detail?device_id=...`: read one device memory value.

The detail response includes key, device id, TTL seconds, message count, raw JSON,
and parsed message summaries. Parsing failures return the raw JSON plus an error.

Redis scanning uses `SCAN` with the configured prefix and a conservative limit so
the endpoint does not block production Redis. The first version does not edit or
delete memory.

## Errors And Security

All memory routes reuse existing admin session authentication. If Redis memory is
not configured, the APIs return an explicit disabled state instead of failing the
whole admin page.

The API never logs Redis values, because memory can contain private prompts and
assistant replies.

## Testing

Tests cover:

- `/admin` links to `/admin/memory`;
- `/admin/memory` renders a standalone memory page;
- memory APIs require auth through the existing admin middleware;
- memory list reads online devices plus Redis keys;
- detail parses Redis string JSON into newest-first summaries by default;
- oldest-first ordering is available;
- Redis disabled and malformed JSON return useful responses.
