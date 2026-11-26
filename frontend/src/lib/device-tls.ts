import type { Device } from "@/lib/api";

// TLS pins. The NMS logs in to a device over API-TLS only when the
// certificate it presents has the fingerprint pinned for it
// (pin_cert_sha256: the SHA-256 of the certificate, which RouterOS prints as
// "fingerprint"). Without a pin a TLS device is not logged in to at all —
// only the liveness check reaches it — so the UI has to say so wherever the
// device is shown.

type TLSDevice = Pick<Device, "use_tls" | "pin_cert_sha256">;

// A TLS device nobody has pinned yet: it is not polled until someone does.
export function tlsPinMissing(d: TLSDevice): boolean {
  return d.use_tls && !d.pin_cert_sha256;
}

// The stored form of a pasted fingerprint: lower-case hex, no colons (the
// backend normalises the same way and is the authority).
export function normalizeFingerprint(input: string): string {
  return input.trim().replaceAll(":", "").toLowerCase();
}

export function isFingerprint(input: string): boolean {
  return /^[0-9a-f]{64}$/.test(normalizeFingerprint(input));
}

// "1a2b3c4d…9e8f7a6b" — enough to recognise a pin without filling a line.
export function shortFingerprint(pin: string): string {
  return pin.length > 20 ? `${pin.slice(0, 8)}…${pin.slice(-8)}` : pin;
}

// Where an admin reads the fingerprint: on the device itself, never from
// what answered at its address.
export const FINGERPRINT_HINT =
  "Read it on the device: /certificate print detail shows the fingerprint of the certificate that /ip service api-ssl uses.";

export const TLS_PIN_MISSING_TEXT =
  "This device is set to API-TLS and no certificate fingerprint is pinned for it, so the NMS does not log in to it: only the liveness check runs.";
