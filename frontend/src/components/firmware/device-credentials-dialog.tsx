"use client";

import { useState, type FormEvent, type ReactNode } from "react";
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
import type { DeviceCredentials } from "@/lib/api";

export interface CredentialsSubmit extends DeviceCredentials {
  reboot: boolean;
}

interface Props {
  open: boolean;
  title: string;
  description: ReactNode;
  confirmLabel: string;
  // Offer "reboot when done" (upgrade, RouterBOARD); undefined hides it.
  rebootLabel?: string;
  onCancel: () => void;
  onSubmit: (values: CredentialsSubmit) => void;
}

// DeviceCredentialsDialog asks for the RouterOS login a firmware action
// uses. The server uses it for that action's sessions only and never stores
// it; here the password lives in component state until submit and is
// cleared then (or on cancel). The user name is kept for the next action on
// the page — in memory only, never in browser storage.
export function DeviceCredentialsDialog({
  open,
  title,
  description,
  confirmLabel,
  rebootLabel,
  onCancel,
  onSubmit,
}: Props) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [reboot, setReboot] = useState(true);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!username.trim() || !password) return;
    const values = { username: username.trim(), password, reboot };
    setPassword("");
    onSubmit(values);
  };

  const cancel = () => {
    setPassword("");
    onCancel();
  };

  return (
    <Dialog open={open} onOpenChange={(next) => !next && cancel()}>
      <DialogContent className="sm:max-w-md">
        <form onSubmit={submit} className="space-y-4" autoComplete="off">
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
            <DialogDescription>{description}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-2">
              <Label htmlFor="fw-username">RouterOS user</Label>
              <Input
                id="fw-username"
                autoComplete="off"
                autoFocus={!username}
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                required
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="fw-password">Password</Label>
              <Input
                id="fw-password"
                type="password"
                autoComplete="new-password"
                autoFocus={!!username}
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                required
              />
              <p className="text-xs text-muted-foreground">
                Used for this action only — not stored. The poll account is never used to change a device.
              </p>
            </div>
            {rebootLabel && (
              <label className="flex items-center gap-2 text-sm">
                <input type="checkbox" checked={reboot} onChange={(e) => setReboot(e.target.checked)} />
                {rebootLabel}
              </label>
            )}
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={cancel}>
              Cancel
            </Button>
            <Button type="submit" disabled={!username.trim() || !password}>
              {confirmLabel}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
