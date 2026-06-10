function getApiBase() {
  if (process.env.NEXT_PUBLIC_API_URL) return process.env.NEXT_PUBLIC_API_URL;
  if (typeof window !== "undefined") {
    // Same logic as src/lib/api.ts: dev / docker-compose backend on :8080,
    // otherwise same-origin through the reverse proxy.
    if (process.env.NODE_ENV === "development" || window.location.port === "3000") {
      return `http://${window.location.hostname}:8080`;
    }
    return "";
  }
  return "http://localhost:8080";
}

function getWsBase() {
  if (process.env.NEXT_PUBLIC_WS_URL) return process.env.NEXT_PUBLIC_WS_URL;
  // A build that bakes only the API URL should keep WS on that same origin
  // rather than silently diverging to the page origin.
  if (process.env.NEXT_PUBLIC_API_URL) return process.env.NEXT_PUBLIC_API_URL.replace(/^http/, "ws");
  if (typeof window !== "undefined") {
    if (process.env.NODE_ENV === "development" || window.location.port === "3000") {
      return `ws://${window.location.hostname}:8080`;
    }
    const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
    return `${proto}//${window.location.host}`;
  }
  return "ws://localhost:8080";
}

type MessageHandler = (topic: string, data: unknown) => void;

// Single-flight refresh promise — apiFetch has the same pattern; both share
// the same /auth/refresh endpoint and the same localStorage entries, so we
// duplicate the deduplication here to avoid a thundering herd from many
// open WS tabs when the access token ages out simultaneously.
let refreshPromise: Promise<string | null> | null = null;
async function refreshAccessToken(): Promise<string | null> {
  if (typeof window === "undefined") return null;
  if (refreshPromise) return refreshPromise;
  const refreshToken = localStorage.getItem("refresh_token");
  if (!refreshToken) return null;
  refreshPromise = (async () => {
    try {
      const res = await fetch(`${getApiBase()}/api/v1/auth/refresh`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ refresh_token: refreshToken }),
      });
      if (!res.ok) {
        window.dispatchEvent(new CustomEvent("auth:expired"));
        return null;
      }
      const tokens = await res.json();
      localStorage.setItem("access_token", tokens.access_token);
      localStorage.setItem("refresh_token", tokens.refresh_token);
      window.dispatchEvent(new CustomEvent("auth:refreshed", { detail: tokens }));
      return tokens.access_token as string;
    } catch {
      return null;
    } finally {
      refreshPromise = null;
    }
  })();
  return refreshPromise;
}

export class NmsWebSocket {
  private ws: WebSocket | null = null;
  private handlers: Map<string, Set<MessageHandler>> = new Map();
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private token: string;
  private disposed = false;
  private refreshAttempted = false;
  // Number of consecutive failed reconnect attempts after a refresh that
  // already succeeded — used to back off when the server is unreachable.
  private failedAttempts = 0;
  private boundOnAuthRefreshed = () => this.onAuthRefreshed();
  private boundOnAuthExpired = () => this.onAuthExpired();

  constructor(token: string) {
    this.token = token;
    if (typeof window !== "undefined") {
      // If apiFetch refreshes the token for us (a REST call hit 401 first),
      // pick up the new value and reconnect without waiting for our own
      // WS-close to detect the expiry.
      window.addEventListener("auth:refreshed", this.boundOnAuthRefreshed);
      window.addEventListener("auth:expired", this.boundOnAuthExpired);
    }
  }

  connect() {
    if (this.disposed) return;
    if (this.ws?.readyState === WebSocket.OPEN) return;

    const currentToken = (typeof window !== "undefined" && localStorage.getItem("access_token")) || this.token;
    this.ws = new WebSocket(`${getWsBase()}/api/v1/ws?token=${currentToken}`);

    this.ws.onopen = () => {
      this.failedAttempts = 0;
      this.refreshAttempted = false;
      // Re-subscribe to all active topics on (re)connect.
      for (const topic of this.handlers.keys()) {
        this.send({ action: "subscribe", topic });
      }
    };

    this.ws.onmessage = (event) => {
      try {
        const msg = JSON.parse(event.data);
        const handlers = this.handlers.get(msg.topic);
        if (handlers) {
          for (const handler of handlers) {
            handler(msg.topic, msg.data);
          }
        }
      } catch {
        // ignore parse errors
      }
    };

    this.ws.onclose = (ev) => {
      if (this.disposed) return;
      // The server rejects an expired/invalid JWT with HTTP 401 during the
      // WebSocket upgrade handshake — browsers surface this as an immediate
      // onclose with code 1006 and no payload. Treat any close that happens
      // before we ever opened (or before we have a server-policy code) as a
      // candidate for token refresh, but only attempt the refresh once per
      // connection cycle so we don't loop on a genuinely revoked token.
      const looksLikeAuth = ev.code === 1008 || (ev.code === 1006 && !this.refreshAttempted);
      if (looksLikeAuth) {
        this.refreshAttempted = true;
        refreshAccessToken().then((newToken) => {
          if (this.disposed) return;
          if (newToken) {
            this.scheduleReconnect(500); // fast retry with the fresh token
          } else {
            this.scheduleReconnect(15000); // refresh failed — back off significantly
          }
        });
        return;
      }
      this.scheduleReconnect();
    };

    this.ws.onerror = () => {
      this.ws?.close();
    };
  }

  private onAuthRefreshed() {
    if (this.disposed) return;
    // The token in localStorage has changed under us. Tear down the existing
    // connection (whatever its state) and reconnect with the new value.
    try { this.ws?.close(); } catch { /* ignore */ }
    this.ws = null;
    this.scheduleReconnect(50);
  }

  private onAuthExpired() {
    if (this.disposed) return;
    // The refresh token is gone or rejected — there's no point pounding the
    // WS endpoint with a token we know won't auth. Stop reconnecting.
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    try { this.ws?.close(); } catch { /* ignore */ }
    this.ws = null;
  }

  subscribe(topic: string, handler: MessageHandler) {
    if (!this.handlers.has(topic)) {
      this.handlers.set(topic, new Set());
      if (this.ws?.readyState === WebSocket.OPEN) {
        this.send({ action: "subscribe", topic });
      }
    }
    this.handlers.get(topic)!.add(handler);

    return () => this.unsubscribe(topic, handler);
  }

  unsubscribe(topic: string, handler: MessageHandler) {
    const handlers = this.handlers.get(topic);
    if (handlers) {
      handlers.delete(handler);
      if (handlers.size === 0) {
        this.handlers.delete(topic);
        if (this.ws?.readyState === WebSocket.OPEN) {
          this.send({ action: "unsubscribe", topic });
        }
      }
    }
  }

  disconnect() {
    this.disposed = true;
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (typeof window !== "undefined") {
      window.removeEventListener("auth:refreshed", this.boundOnAuthRefreshed);
      window.removeEventListener("auth:expired", this.boundOnAuthExpired);
    }
    this.ws?.close();
    this.ws = null;
  }

  private send(data: object) {
    this.ws?.send(JSON.stringify(data));
  }

  private scheduleReconnect(delayMs?: number) {
    if (this.disposed) return;
    if (this.reconnectTimer) return;
    // Default backoff: 3s, with a 1.5× ramp per consecutive failure up to 30s.
    if (delayMs == null) {
      this.failedAttempts++;
      delayMs = Math.min(30000, 3000 * Math.pow(1.5, Math.min(this.failedAttempts - 1, 8)));
    }
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect();
    }, delayMs);
  }
}
