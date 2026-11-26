"use client";

import { useCallback, useState } from "react";
import { AlertTriangle, Check, Copy } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { useAuth } from "@/context/auth";
import { api, type FlowRollbackStep, type FlowSetup } from "@/lib/api";
import { usePolledFetch } from "../hooks";
import { Notice } from "../notice";
import { copyText } from "./flow-lib";

export function CodeBlock({ title, lines }: { title: string; lines: string[] }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    const ok = await copyText(lines.join("\n"));
    if (ok) {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } else toast.error("Copy failed — select the text instead");
  };
  return (
    <div className="min-w-0 space-y-1">
      <div className="flex items-center justify-between gap-2">
        <span className="text-xs font-medium">{title}</span>
        <Button size="xs" variant="ghost" onClick={copy} aria-label={`Copy ${title}`}>
          {copied ? <Check /> : <Copy />}
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
      <pre className="overflow-x-auto rounded-md border bg-muted/40 px-3 py-2 font-mono text-[11px] leading-relaxed whitespace-pre">
        {lines.join("\n")}
      </pre>
    </div>
  );
}

// RollbackSteps: how to stop a device's flow export, as numbered steps. The
// steps are deliberately NOT one copyable block: the order matters, step 0
// decides whether the later ones are needed at all, and before RouterOS 7.25
// one of them can crash a busy device. Each step's commands copy separately.
export function RollbackSteps({ plan }: { plan: FlowSetup["routeros"]["rollback"] }) {
  return (
    <div className="min-w-0 space-y-1.5">
      <span className="text-xs font-medium">Rollback (stop the export) — one step at a time</span>
      {plan.note && <p className="text-[11px] text-muted-foreground">{plan.note}</p>}
      <ol className="space-y-2">
        {plan.steps.map((s, i) => (
          <RollbackStep key={i} n={i} step={s} />
        ))}
      </ol>
    </div>
  );
}

function RollbackStep({ n, step }: { n: number; step: FlowRollbackStep }) {
  return (
    <li className="min-w-0 rounded-md border px-3 py-2 text-xs">
      <p>
        <span className="mr-1.5 font-mono font-medium">{n}.</span>
        {step.optional && <span className="mr-1.5 rounded-full border px-1.5 text-[10px] text-muted-foreground">only if needed</span>}
        {step.text}
      </p>
      {step.warning && (
        <p className="mt-1.5 flex items-start gap-1.5 rounded-md border border-amber-500/30 bg-amber-500/5 px-2 py-1.5 text-amber-800 dark:text-amber-300">
          <AlertTriangle className="mt-px h-3.5 w-3.5 shrink-0" />
          <span>{step.warning}</span>
        </p>
      )}
      {step.commands.length > 0 && (
        <div className="mt-1.5">
          <CodeBlock title={`Step ${n}`} lines={step.commands} />
        </div>
      )}
    </li>
  );
}

// StopExportDialog: the read-only check and the rollback for one device —
// what an operator needs when a device that must not export is seen
// exporting. It shows text only; nothing is sent to the device.
export function StopExportDialog({ deviceId, deviceName, onClose }: { deviceId: string | null; deviceName: string; onClose: () => void }) {
  const { token } = useAuth();
  const fetcher = useCallback(() => api.flows.setup(token!, deviceId ?? undefined), [token, deviceId]);
  const { data, error } = usePolledFetch(token && deviceId ? `fstop|${deviceId}` : null, fetcher, null, { group: "fstop" });
  const current = data && data.routeros.device_id === deviceId ? data : null;
  return (
    <Dialog open={deviceId !== null} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>Stop the flow export on {deviceName}</DialogTitle>
          <DialogDescription>
            Run these on the device yourself (WinBox terminal or SSH). The NMS only shows them — ignoring or deleting the
            exporter here does not stop the device from exporting.
          </DialogDescription>
        </DialogHeader>
        {error && !current && <Notice kind="error">Could not load the rollback: {error}</Notice>}
        {!current && !error && <div className="h-32 animate-pulse rounded-md bg-muted" />}
        {current && (
          <div className="space-y-3">
            {current.role_reason && <Notice kind="warn">{current.role_reason}.</Notice>}
            <CodeBlock title="Check first (read-only)" lines={current.routeros.check} />
            <RollbackSteps plan={current.routeros.rollback} />
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}
