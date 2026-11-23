"use client";

import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { ArrowRight, Loader2, MapPinned } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { ReauthCancelled, useFreshAuth } from "@/components/auth/fresh-auth";
import { useNow } from "@/hooks/use-now";
import { api, type AddressProposal } from "@/lib/api";
import { fmtAgo } from "@/lib/device-load";

// Address change detected: neighbor discovery saw a managed device's MAC at
// another IP. The NMS does not contact the new address on its own — MNDP is
// unauthenticated, and verifying means sending the stored password there —
// so an admin approves (log in there, check board/version/MAC, re-point) or
// dismisses each proposal. Viewers see the list read-only. Renders nothing
// while no proposal is pending.

const REFRESH_MS = 60_000;

function ago(iso: string, nowMs: number): string {
  return fmtAgo(Math.max(0, (nowMs - Date.parse(iso)) / 1000));
}

export function AddressProposalsPanel({
  token,
  isAdmin,
  onDeviceMoved,
}: {
  token: string;
  isAdmin: boolean;
  onDeviceMoved?: () => void;
}) {
  const withFreshAuth = useFreshAuth();
  const now = useNow(30_000);
  const [proposals, setProposals] = useState<AddressProposal[]>([]);
  const [busy, setBusy] = useState<number | null>(null);
  const [confirm, setConfirm] = useState<AddressProposal | null>(null);

  const load = useCallback(() => {
    api.devices
      .addressProposals(token)
      .then(setProposals)
      .catch(() => {
        // The panel is an aside: a failed read leaves the last list.
      });
  }, [token]);

  useEffect(() => {
    load();
    const t = setInterval(load, REFRESH_MS);
    return () => clearInterval(t);
  }, [load]);

  const approve = async (p: AddressProposal) => {
    setConfirm(null);
    setBusy(p.id);
    try {
      const res = await withFreshAuth((t) => api.devices.approveAddressProposal(t, p.id));
      if (res.status === "approved") {
        toast.success(`${p.device_identity || p.device_id} now polled at ${p.new_address}`);
        onDeviceMoved?.();
      } else {
        toast.error(`Move refused: ${res.result || res.status}`, { duration: 10_000 });
      }
    } catch (err) {
      if (!(err instanceof ReauthCancelled)) {
        toast.error(err instanceof Error ? err.message : "Approve failed");
      }
    } finally {
      setBusy(null);
      load();
    }
  };

  const dismiss = async (p: AddressProposal) => {
    setBusy(p.id);
    try {
      await api.devices.dismissAddressProposal(token, p.id);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Dismiss failed");
    } finally {
      setBusy(null);
      load();
    }
  };

  if (proposals.length === 0) return null;

  return (
    <Card className="border-amber-500/40 bg-amber-500/5">
      <CardHeader className="pb-2">
        <CardTitle className="flex items-center gap-2 text-base">
          <MapPinned className="h-4 w-4 text-amber-600 dark:text-amber-400" aria-hidden />
          Address change detected
        </CardTitle>
        <CardDescription>
          Neighbor discovery saw {proposals.length === 1 ? "a device" : `${proposals.length} devices`} at a new
          address. Nothing has been contacted there yet
          {isAdmin ? " — approve to move the device, or dismiss (the same move stays quiet for 7 days)." : "; an admin can approve or dismiss the move."}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-2">
        {proposals.map((p) => (
          <div
            key={p.id}
            className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2 rounded-md border bg-background px-3 py-2 text-sm"
          >
            <div className="min-w-0 space-y-0.5">
              <div className="flex flex-wrap items-center gap-x-2">
                <Link href={`/devices/${p.device_id}`} className="font-medium hover:underline">
                  {p.device_identity || p.device_id}
                </Link>
                <span className="inline-flex items-center gap-1 font-mono text-xs">
                  {p.old_address}
                  <ArrowRight className="h-3 w-3 text-muted-foreground" aria-label="to" />
                  {p.new_address}
                </span>
              </div>
              <p className="text-xs text-muted-foreground">
                {p.mac && <span className="font-mono">{p.mac}</span>}
                {p.mac && " · "}first seen {ago(p.first_seen, now)}, last seen {ago(p.last_seen, now)}
              </p>
            </div>
            {isAdmin && (
              <div className="flex gap-2">
                <Button size="sm" variant="outline" disabled={busy !== null} onClick={() => dismiss(p)}>
                  Dismiss
                </Button>
                <Button size="sm" disabled={busy !== null} onClick={() => setConfirm(p)}>
                  {busy === p.id && <Loader2 className="mr-1 h-3.5 w-3.5 animate-spin" />}
                  Approve…
                </Button>
              </div>
            )}
          </div>
        ))}
      </CardContent>

      <Dialog open={confirm !== null} onOpenChange={(open) => !open && setConfirm(null)}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>Move {confirm?.device_identity || confirm?.device_id}?</DialogTitle>
            <DialogDescription>
              The NMS will log in at <span className="font-mono">{confirm?.new_address}</span> with this
              device&apos;s stored username and password, and move the device there only if the same board,
              RouterOS version and MAC answer. The address came from neighbor discovery, which anyone on the
              network can fake — approve only if you expect the device to be there.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setConfirm(null)}>
              Cancel
            </Button>
            <Button onClick={() => confirm && approve(confirm)}>Log in and move</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  );
}
