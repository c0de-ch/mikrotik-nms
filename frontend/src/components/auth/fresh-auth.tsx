"use client";

import { createContext, useCallback, useContext, useRef, useState, type FormEvent, type ReactNode } from "react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useAuth } from "@/context/auth";
import { ApiError } from "@/lib/api";

// Thrown by withFreshAuth when the user closes the password dialog: the
// action did not run, and there is nothing to report.
export class ReauthCancelled extends Error {
  constructor() {
    super("cancelled");
    this.name = "ReauthCancelled";
  }
}

type WithFreshAuth = <T>(action: (token: string) => Promise<T>) => Promise<T>;

const FreshAuthContext = createContext<WithFreshAuth | null>(null);

// FreshAuthProvider runs actions that change a device (firmware upgrades).
// The server only accepts those from a session whose password was typed in
// the last few minutes and answers 403 reauth_required otherwise; the
// provider then asks for the password, logs in again and repeats the action
// once with the new token.
export function FreshAuthProvider({ children }: { children: ReactNode }) {
  const { token, user, reauthenticate } = useAuth();
  const [open, setOpen] = useState(false);
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // The action waiting for the dialog (at most one: the dialog is modal).
  const waiting = useRef<{ resolve: (token: string) => void; reject: (err: unknown) => void } | null>(null);

  const close = () => {
    waiting.current = null;
    setOpen(false);
    setPassword("");
    setError(null);
  };

  const withFreshAuth = useCallback<WithFreshAuth>(
    async (action) => {
      if (!token) throw new Error("Not signed in");
      try {
        return await action(token);
      } catch (err) {
        if (!(err instanceof ApiError && err.code === "reauth_required")) throw err;
      }
      const fresh = await new Promise<string>((resolve, reject) => {
        waiting.current?.reject(new ReauthCancelled());
        waiting.current = { resolve, reject };
        setOpen(true);
      });
      return action(fresh);
    },
    [token],
  );

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!password || busy) return;
    setBusy(true);
    setError(null);
    try {
      const fresh = await reauthenticate(password);
      waiting.current?.resolve(fresh);
      close();
    } catch (err) {
      setError(
        err instanceof ApiError && err.status === 401
          ? "Wrong password"
          : err instanceof Error
            ? err.message
            : "Sign-in failed",
      );
    } finally {
      setBusy(false);
    }
  };

  const cancel = () => {
    waiting.current?.reject(new ReauthCancelled());
    close();
  };

  return (
    <FreshAuthContext.Provider value={withFreshAuth}>
      {children}
      <Dialog open={open} onOpenChange={(next) => !next && cancel()}>
        <DialogContent className="sm:max-w-sm">
          <form onSubmit={submit} className="space-y-4">
            <DialogHeader>
              <DialogTitle>Confirm it&apos;s you</DialogTitle>
              <DialogDescription>
                This action changes devices, so it needs your password again
                {user?.username ? ` (${user.username})` : ""}.
              </DialogDescription>
            </DialogHeader>
            <div className="space-y-2">
              <Label htmlFor="reauth-password">Password</Label>
              <Input
                id="reauth-password"
                type="password"
                autoComplete="current-password"
                autoFocus
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                required
              />
              {error && <p className="text-sm text-destructive">{error}</p>}
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={cancel} disabled={busy}>
                Cancel
              </Button>
              <Button type="submit" disabled={busy || !password}>
                {busy ? "Checking…" : "Continue"}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </FreshAuthContext.Provider>
  );
}

// useFreshAuth returns withFreshAuth(action): run an action that may need a
// fresh login. action receives the access token to use.
export function useFreshAuth(): WithFreshAuth {
  const ctx = useContext(FreshAuthContext);
  if (!ctx) throw new Error("useFreshAuth must be used within FreshAuthProvider");
  return ctx;
}
