---
paths:
  - "web/**"
---

# Web app

- Vite + React + TypeScript. Keep the UI simple: the backend is the focus of this project.
- The canvas captures pointer events and sends stroke points in batches. The batch interval comes from server config (default 50 ms) so experiments can tune it.
- The client never decides game outcomes. It renders what the server sends and applies room events in `seq` order.
- On disconnect, reconnect with backoff and send the last `seq` received so the server can resume the stream.
- Keep the access token in memory only. The refresh token lives in an HttpOnly cookie set by `auth`.
- Report Core Web Vitals (LCP, INP, CLS) and WebSocket round-trip time to the OpenTelemetry collector.
- Money actions (buy, list) send an `Idempotency-Key` header that is generated once per user action and reused on retry.
