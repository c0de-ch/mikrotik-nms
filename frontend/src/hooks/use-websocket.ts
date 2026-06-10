"use client";

import { useEffect, useRef, useCallback } from "react";
import { NmsWebSocket } from "@/lib/ws";
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

export function useWebSocket(topic: string, handler: (data: unknown) => void) {
  const { token } = useAuth();
  const handlerRef = useRef(handler);
  // Keep the latest handler in a ref (updated post-render, not during render)
  // so the WS subscription itself never has to resubscribe on handler change.
  useEffect(() => {
    handlerRef.current = handler;
  }, [handler]);

  const stableHandler = useCallback((_topic: string, data: unknown) => {
    handlerRef.current(data);
  }, []);

  useEffect(() => {
    if (!token) return;

    const ws = getSharedWs(token);
    const unsub = ws.subscribe(topic, stableHandler);

    return () => {
      unsub();
      releaseSharedWs();
    };
  }, [token, topic, stableHandler]);
}
