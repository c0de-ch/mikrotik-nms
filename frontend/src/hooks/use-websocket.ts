"use client";

import { useEffect, useRef, useCallback, useSyncExternalStore } from "react";
import { NmsWebSocket, type ControlType } from "@/lib/ws";
import { useAuth } from "@/context/auth";

let sharedWs: NmsWebSocket | null = null;
let refCount = 0;

function getSharedWs(token: string): NmsWebSocket {
  if (!sharedWs) {
    sharedWs = new NmsWebSocket(token);
    sharedWs.connect();
  }
  refCount++;
  return sharedWs;
}

function releaseSharedWs() {
  refCount--;
  if (refCount <= 0) {
    sharedWs?.disconnect();
    sharedWs = null;
    refCount = 0;
  }
}

function subscribeVisibility(onChange: () => void) {
  document.addEventListener("visibilitychange", onChange);
  return () => document.removeEventListener("visibilitychange", onChange);
}

// usePageVisible is false while the browser tab is hidden (another tab in
// front, the window minimised).
export function usePageVisible(): boolean {
  return useSyncExternalStore(
    subscribeVisibility,
    () => document.visibilityState !== "hidden",
    () => true,
  );
}

// useWebSocket subscribes to a topic on the shared connection. onStatus
// receives the topic's control frames (subscribe.ack / subscribe.error),
// which only vetted topics such as the per-port live stream send.
//
// pauseWhenHidden drops the subscription while the tab is hidden and takes
// it up again when it shows: for a topic whose subscribers make a device
// work (the per-port stream samples the device for as long as anyone is
// subscribed), so a tab left open in the background costs the device
// nothing.
export function useWebSocket(
  topic: string,
  handler: (data: unknown) => void,
  onStatus?: (type: ControlType, data: unknown) => void,
  options?: { pauseWhenHidden?: boolean },
) {
  const { token } = useAuth();
  const visible = usePageVisible();
  const activeTopic = options?.pauseWhenHidden && !visible ? "" : topic;
  const handlerRef = useRef(handler);
  const statusRef = useRef(onStatus);
  // Keep the latest handlers in refs (updated post-render, not during render)
  // so the WS subscription itself never has to resubscribe on handler change.
  useEffect(() => {
    handlerRef.current = handler;
    statusRef.current = onStatus;
  }, [handler, onStatus]);

  const stableHandler = useCallback((_topic: string, data: unknown) => {
    handlerRef.current(data);
  }, []);
  const stableStatus = useCallback((type: ControlType, data: unknown) => {
    statusRef.current?.(type, data);
  }, []);

  useEffect(() => {
    // An empty topic means "not subscribed right now" (e.g. a live stream
    // that only runs for one range, or a paused one) — don't open or hold
    // the connection.
    if (!token || !activeTopic) return;

    const ws = getSharedWs(token);
    const unsub = ws.subscribe(activeTopic, stableHandler, stableStatus);

    return () => {
      unsub();
      releaseSharedWs();
    };
  }, [token, activeTopic, stableHandler, stableStatus]);
}
