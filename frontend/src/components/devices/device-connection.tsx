"use client";

import { useId, useState } from "react";
import { Pencil, ShieldAlert } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api, type Device } from "@/lib/api";
import {
  FINGERPRINT_HINT,
  TLS_PIN_MISSING_TEXT,
  isFingerprint,
  normalizeFingerprint,
  tlsPinMissing,
} from "@/lib/device-tls";
import { cn } from "@/lib/utils";

// How the NMS reaches a device's API: the pieces shared by the add-device
// form, the edit dialog and every place a TLS device without a pin has to
// be called out.

const PLAIN_PORT = "8728";
const TLS_PORT = "8729";

// TLSPinMissingBadge marks a TLS device the NMS is not logging in to because
// nobody has pinned its certificate yet. Renders nothing for any other
// device.
export function TLSPinMissingBadge({
  device,
  className,
}: {
  device: Pick<Device, "use_tls" | "pin_cert_sha256">;
  className?: string;
}) {
  if (!tlsPinMissing(device)) return null;
  return (
    <span
      title={TLS_PIN_MISSING_TEXT}
      className={cn(
        "inline-flex h-5 items-center gap-1 whitespace-nowrap rounded-full bg-amber-500/15 px-2 text-xs font-medium text-amber-800 dark:text-amber-300",
        className,
      )}
    >
      <ShieldAlert aria-hidden className="h-3 w-3 shrink-0" />
      TLS pin missing
    </span>
  );
}

// TLSFields: the API-TLS switch and, while it is on, the certificate
// fingerprint with one line on where to read it. Turning TLS on or off moves
// a port that is still the other mode's default.
export function TLSFields({
  useTLS,
  pin,
  port,
  onChange,
}: {
  useTLS: boolean;
  pin: string;
  port: string;
  onChange: (next: { useTLS: boolean; pin: string; port: string }) => void;
}) {
  const id = useId();
  const invalid = pin.trim() !== "" && !isFingerprint(pin);
  return (
    <div className="space-y-3">
      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          className="accent-primary"
          checked={useTLS}
          onChange={(e) => {
            const on = e.target.checked;
            const nextPort = on && port === PLAIN_PORT ? TLS_PORT : !on && port === TLS_PORT ? PLAIN_PORT : port;
            onChange({ useTLS: on, pin, port: nextPort });
          }}
        />
        Connect over API-TLS (the device&apos;s api-ssl service)
      </label>
      {useTLS && (
        <div className="space-y-2">
          <Label htmlFor={`${id}-pin`}>Certificate fingerprint (SHA-256)</Label>
          <Input
            id={`${id}-pin`}
            value={pin}
            onChange={(e) => onChange({ useTLS, pin: e.target.value, port })}
            required
            spellCheck={false}
            autoComplete="off"
            autoCapitalize="off"
            placeholder="64 hex digits — colons and upper case are fine"
            className="font-mono text-xs"
            aria-invalid={invalid || undefined}
            aria-describedby={`${id}-pin-hint`}
          />
          <p id={`${id}-pin-hint`} className="text-xs text-muted-foreground">
            {FINGERPRINT_HINT} The NMS logs in only to a device that presents this certificate.
          </p>
          {invalid && (
            <p role="alert" className="text-xs text-red-700 dark:text-red-400">
              A SHA-256 fingerprint is 64 hex digits.
            </p>
          )}
        </div>
      )}
    </div>
  );
}

// EditConnectionDialog changes how the NMS reaches one device — address,
// port, login, API-TLS and the pinned fingerprint. Saving drops the device's
// current API session, so the next poll connects with the new values.
export function EditConnectionDialog({
  device,
  token,
  onSaved,
}: {
  device: Device;
  token: string;
  onSaved: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [saving, setSaving] = useState(false);
  const initial = () => ({
    address: device.address,
    username: device.username,
    password: "",
    port: String(device.api_port || PLAIN_PORT),
    useTLS: device.use_tls,
    pin: device.pin_cert_sha256 ?? "",
  });
  const [form, setForm] = useState(initial);

  const save = async (e: React.FormEvent) => {
    e.preventDefault();
    if (form.useTLS && !isFingerprint(form.pin)) {
      toast.error("Enter the certificate fingerprint: 64 hex digits");
      return;
    }
    setSaving(true);
    try {
      await api.devices.update(token, device.id, {
        address: form.address.trim(),
        username: form.username.trim(),
        api_port: parseInt(form.port) || undefined,
        use_tls: form.useTLS,
        // A pin typed while TLS is off is kept for the day it is switched on.
        pin_cert_sha256: form.pin.trim() === "" || isFingerprint(form.pin) ? normalizeFingerprint(form.pin) : undefined,
        ...(form.password ? { password: form.password } : {}),
      });
      toast.success("Connection saved — the NMS reconnects with the new settings");
      setOpen(false);
      onSaved();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed to save the connection");
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        // Always start from the device as it is now; never keep a typed
        // password around after the dialog closes.
        setForm(initial());
        setOpen(next);
      }}
    >
      <DialogTrigger render={<Button variant="outline" aria-label="Edit the API connection" />}>
        <Pencil className="h-4 w-4 sm:mr-1" />
        <span className="hidden sm:inline">Connection</span>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>API connection</DialogTitle>
        </DialogHeader>
        <form onSubmit={save} className="space-y-4">
          <div className="grid grid-cols-3 gap-4">
            <div className="col-span-2 space-y-2">
              <Label htmlFor="conn-address">Address</Label>
              <Input id="conn-address" value={form.address} onChange={(e) => setForm({ ...form, address: e.target.value })} required />
            </div>
            <div className="space-y-2">
              <Label htmlFor="conn-port">API port</Label>
              <Input id="conn-port" inputMode="numeric" value={form.port} onChange={(e) => setForm({ ...form, port: e.target.value })} required />
            </div>
          </div>
          <div className="grid grid-cols-2 gap-4">
            <div className="space-y-2">
              <Label htmlFor="conn-user">Username</Label>
              <Input id="conn-user" value={form.username} onChange={(e) => setForm({ ...form, username: e.target.value })} required autoComplete="off" />
            </div>
            <div className="space-y-2">
              <Label htmlFor="conn-pass">Password</Label>
              <Input
                id="conn-pass"
                type="password"
                value={form.password}
                onChange={(e) => setForm({ ...form, password: e.target.value })}
                placeholder="unchanged"
                autoComplete="new-password"
              />
            </div>
          </div>
          <TLSFields
            useTLS={form.useTLS}
            pin={form.pin}
            port={form.port}
            onChange={({ useTLS, pin, port }) => setForm({ ...form, useTLS, pin, port })}
          />
          <Button type="submit" className="w-full" disabled={saving}>
            {saving ? "Saving…" : "Save connection"}
          </Button>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// TLSPinMissingNotice is the device page's explanation of why a TLS device
// without a pin shows no data, and what to do about it.
export function TLSPinMissingNotice({ device, isAdmin }: { device: Device; isAdmin: boolean }) {
  if (!tlsPinMissing(device)) return null;
  return (
    <div role="status" className="flex items-start gap-3 rounded-lg border border-amber-500/40 bg-amber-500/10 p-4">
      <ShieldAlert aria-hidden className="mt-0.5 h-5 w-5 shrink-0 text-amber-700 dark:text-amber-300" />
      <div className="min-w-0 space-y-1 text-sm">
        <p className="font-semibold text-amber-900 dark:text-amber-200">TLS pin missing — this device is not polled</p>
        <p className="text-amber-900/90 dark:text-amber-100/90">{TLS_PIN_MISSING_TEXT}</p>
        <p className="text-amber-900/90 dark:text-amber-100/90">
          {isAdmin
            ? `Paste the fingerprint under Connection. ${FINGERPRINT_HINT}`
            : "An admin has to enter the fingerprint of the device's certificate."}
        </p>
      </div>
    </div>
  );
}
