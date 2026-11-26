"use client";

import { useCallback, useEffect, useState, type FormEvent } from "react";
import Link from "next/link";
import { BellOff, BookOpen, Check, Loader2, Plus, Send, Trash2 } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { useAuth } from "@/context/auth";
import { useNow } from "@/hooks/use-now";
import { useWebSocket } from "@/hooks/use-websocket";
import {
  api,
  type Alert,
  type AlertList,
  type AlertSeverity,
  type AlertSilence,
  type NotificationChannel,
} from "@/lib/api";
import { toast } from "sonner";

const RULE_LABELS: Record<string, string> = {
  device_unreachable: "Device unreachable",
  parallel_api_poller: "Another API poller",
  flow_export_on_switch: "Flow export on a device that must not export",
};

function ruleLabel(rule: string): string {
  return RULE_LABELS[rule] ?? rule;
}

// A fact that is a list of text lines (the rollback steps, the check
// commands a rule attaches to its alert).
function factLines(a: Alert, key: string): string[] {
  const v = a.facts?.[key];
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];
}

// The steps a rule attached to its alert, folded away under the summary.
function AlertSteps({ title, lines }: { title: string; lines: string[] }) {
  if (lines.length === 0) return null;
  return (
    <details className="mt-1.5 text-xs">
      <summary className="cursor-pointer text-muted-foreground hover:text-foreground">{title}</summary>
      <pre className="mt-1 overflow-x-auto rounded-md border bg-muted/40 px-3 py-2 font-mono text-[11px] leading-relaxed whitespace-pre-wrap">
        {lines.join("\n")}
      </pre>
    </details>
  );
}

// "12 min", "3 h 05 min", "2 d 4 h" between two instants.
function span(fromMs: number, toMs: number): string {
  const mins = Math.max(0, Math.floor((toMs - fromMs) / 60_000));
  if (mins < 1) return "under a minute";
  if (mins < 60) return `${mins} min`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours} h ${String(mins % 60).padStart(2, "0")} min`;
  return `${Math.floor(hours / 24)} d ${hours % 24} h`;
}

function SeverityBadge({ severity }: { severity: AlertSeverity }) {
  const cls =
    severity === "critical"
      ? "border-transparent bg-destructive text-white"
      : severity === "warning"
        ? "border-transparent bg-amber-500 text-white"
        : "";
  return (
    <Badge variant="outline" className={cls}>
      {severity}
    </Badge>
  );
}

function silenceLabel(s: AlertSilence, deviceNames: Map<string, string>): string {
  if (s.scope === "site") return "Everything";
  if (s.scope === "rule") return `Rule: ${ruleLabel(s.target)}`;
  return `Device: ${deviceNames.get(s.target) ?? s.target}`;
}

export default function AlertsPage() {
  const { token, user } = useAuth();
  const admin = user?.role === "admin";
  const now = useNow(30_000);
  const [data, setData] = useState<AlertList | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [channels, setChannels] = useState<NotificationChannel[] | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const load = useCallback(async () => {
    if (!token) return;
    try {
      setData(await api.alerts.list(token));
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load alerts");
    }
  }, [token]);

  const loadChannels = useCallback(async () => {
    if (!token || !admin) return;
    try {
      setChannels(await api.alerts.channels(token));
    } catch {
      setChannels([]);
    }
  }, [token, admin]);

  useEffect(() => {
    if (!token) return;
    // Deferred a tick so the effect itself never sets state.
    const first = setTimeout(() => {
      load();
      loadChannels();
    }, 0);
    const timer = setInterval(load, 60_000);
    return () => {
      clearTimeout(first);
      clearInterval(timer);
    };
  }, [token, load, loadChannels]);

  useWebSocket("alerts.update", load);

  // One action at a time; every action ends with a fresh list.
  const run = async (key: string, action: () => Promise<unknown>, done?: string) => {
    if (!token || busy) return;
    setBusy(key);
    try {
      await action();
      if (done) toast.success(done);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "Failed");
    } finally {
      setBusy(null);
      load();
    }
  };

  const open = data?.alerts.filter((a) => a.state !== "resolved") ?? [];
  const resolved = data?.alerts.filter((a) => a.state === "resolved") ?? [];
  const deviceNames = new Map<string, string>();
  for (const a of data?.alerts ?? []) {
    if (a.device_id && a.device_name) deviceNames.set(a.device_id, a.device_name);
  }

  const stateCell = (a: Alert) => (
    <div className="flex flex-wrap items-center gap-1.5">
      {a.state === "acked" ? (
        <Badge variant="secondary">acknowledged{a.acked_by ? ` by ${a.acked_by}` : ""}</Badge>
      ) : (
        <Badge variant="outline">firing</Badge>
      )}
      {a.silenced && (
        <Badge variant="outline" className="text-muted-foreground">
          <BellOff className="mr-1 h-3 w-3" />
          silenced
        </Badge>
      )}
      {a.state === "firing" && !a.silenced && !a.last_notified_at && (
        <span className="text-xs text-muted-foreground">not notified yet</span>
      )}
    </div>
  );

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold">Alerts</h1>
        <p className="text-xs text-muted-foreground">
          {data
            ? `${data.firing} firing, ${data.acked} acknowledged — evaluated every 30 seconds from what the pollers store`
            : "Loading…"}
          {error && <span className="ml-2 text-destructive">{error}</span>}
        </p>
      </div>

      {data && !data.evaluating && (
        <Card>
          <CardContent className="py-4 text-sm text-muted-foreground">
            Polling is off on this instance, so the rules are not evaluated here: what you see is what the database
            held when it was copied.
          </CardContent>
        </Card>
      )}

      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-base">Open</CardTitle>
        </CardHeader>
        <CardContent>
          {data && open.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted-foreground">Nothing is firing.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Severity</TableHead>
                  <TableHead>What</TableHead>
                  <TableHead>For</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {open.map((a) => (
                  <TableRow key={a.id}>
                    <TableCell>
                      <SeverityBadge severity={a.severity} />
                    </TableCell>
                    <TableCell className="max-w-md whitespace-normal">
                      <div className="text-sm font-medium">{a.summary || `${a.rule} ${a.key}`}</div>
                      <div className="text-xs text-muted-foreground">
                        {ruleLabel(a.rule)}
                        {a.device_id && (
                          <>
                            {" · "}
                            <Link href={`/devices/${a.device_id}`} className="underline-offset-2 hover:underline">
                              {a.device_name || "device"}
                            </Link>
                          </>
                        )}
                      </div>
                      {a.confirmed === false && (
                        <Badge
                          variant="outline"
                          className="mt-1 text-muted-foreground"
                          title="Raised on evidence that can be forged or mistaken; it turns critical only once it is confirmed"
                        >
                          unconfirmed
                        </Badge>
                      )}
                      <AlertSteps title="Rollback steps (run them on the device)" lines={factLines(a, "rollback")} />
                      {a.confirmed === false && <AlertSteps title="Check by hand (read-only)" lines={factLines(a, "check")} />}
                    </TableCell>
                    <TableCell
                      className="whitespace-nowrap text-sm text-muted-foreground"
                      title={new Date(a.first_at).toLocaleString()}
                    >
                      {now > 0 ? span(Date.parse(a.first_at), now) : "—"}
                    </TableCell>
                    <TableCell>{stateCell(a)}</TableCell>
                    <TableCell>
                      <div className="flex flex-wrap justify-end gap-1.5">
                        <Button
                          variant="ghost"
                          size="sm"
                          render={<a href={a.runbook_url} target="_blank" rel="noreferrer" />}
                          title="What to check and do"
                        >
                          <BookOpen className="mr-1 h-3.5 w-3.5" />
                          Runbook
                        </Button>
                        {admin && a.state === "firing" && (
                          <Button
                            variant="outline"
                            size="sm"
                            disabled={!!busy}
                            title="Keep the alert open but stop re-notifying"
                            onClick={() => run(`ack-${a.id}`, () => api.alerts.ack(token!, a.id))}
                          >
                            <Check className="mr-1 h-3.5 w-3.5" />
                            Acknowledge
                          </Button>
                        )}
                        {admin && a.device_id && !a.silenced && (
                          <>
                            {[1, 24].map((hours) => (
                              <Button
                                key={hours}
                                variant="outline"
                                size="sm"
                                disabled={!!busy}
                                title={`No notifications about this device for ${hours} h`}
                                onClick={() =>
                                  run(`silence-${a.id}-${hours}`, () =>
                                    api.alerts.silence(token!, { scope: "device", target: a.device_id!, hours }),
                                  )
                                }
                              >
                                <BellOff className="mr-1 h-3.5 w-3.5" />
                                {hours} h
                              </Button>
                            ))}
                          </>
                        )}
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {data && data.silences.length > 0 && (
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-base">Silences</CardTitle>
          </CardHeader>
          <CardContent>
            <p className="mb-3 text-xs text-muted-foreground">
              A silence stops notifications, not the alert. When it ends, anything still firing is announced.
            </p>
            <Table>
              <TableBody>
                {data.silences.map((s) => (
                  <TableRow key={s.id}>
                    <TableCell className="text-sm">{silenceLabel(s, deviceNames)}</TableCell>
                    <TableCell className="text-sm text-muted-foreground" title={new Date(s.until).toLocaleString()}>
                      {now > 0 ? `${span(now, Date.parse(s.until))} left` : "—"}
                    </TableCell>
                    <TableCell className="text-sm text-muted-foreground">
                      {s.reason || "—"} · {s.created_by}
                    </TableCell>
                    <TableCell className="text-right">
                      {admin && (
                        <Button
                          variant="ghost"
                          size="sm"
                          disabled={!!busy}
                          onClick={() => run(`unsilence-${s.id}`, () => api.alerts.endSilence(token!, s.id))}
                        >
                          End now
                        </Button>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}

      {admin && (
        <ChannelsCard
          channels={channels}
          busy={busy}
          onChanged={loadChannels}
          run={run}
          token={token}
        />
      )}

      {resolved.length > 0 && (
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-base">Resolved in the last 7 days</CardTitle>
          </CardHeader>
          <CardContent>
            <Table>
              <TableBody>
                {resolved.map((a) => (
                  <TableRow key={a.id} className="text-muted-foreground">
                    <TableCell>
                      <SeverityBadge severity={a.severity} />
                    </TableCell>
                    <TableCell className="max-w-md whitespace-normal text-sm">{a.summary || `${a.rule} ${a.key}`}</TableCell>
                    <TableCell className="whitespace-nowrap text-sm">{new Date(a.first_at).toLocaleString()}</TableCell>
                    <TableCell className="whitespace-nowrap text-sm">
                      {a.resolved_at ? `lasted ${span(Date.parse(a.first_at), Date.parse(a.resolved_at))}` : ""}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </CardContent>
        </Card>
      )}
    </div>
  );
}

// ChannelsCard: where notifications go (admin). A channel's address or URL
// is write-only — the server shows a hint of it, never the value.
function ChannelsCard({
  channels,
  busy,
  onChanged,
  run,
  token,
}: {
  channels: NotificationChannel[] | null;
  busy: string | null;
  onChanged: () => void;
  run: (key: string, action: () => Promise<unknown>, done?: string) => Promise<void>;
  token: string | null;
}) {
  const [kind, setKind] = useState<"smtp" | "webhook">("smtp");
  const [name, setName] = useState("");
  const [target, setTarget] = useState("");
  const [minSeverity, setMinSeverity] = useState<AlertSeverity>("warning");

  const add = async (e: FormEvent) => {
    e.preventDefault();
    if (!token) return;
    await run(
      "channel-add",
      async () => {
        await api.alerts.createChannel(token, { name, kind, target, min_severity: minSeverity });
        setName("");
        setTarget("");
        onChanged();
      },
      "Channel added — send a test to check it",
    );
  };

  const test = (c: NotificationChannel) =>
    run(`channel-test-${c.id}`, async () => {
      const r = await api.alerts.testChannel(token!, c.id);
      if (r.ok) toast.success(r.message);
      else toast.error(r.message, { duration: 10_000 });
    });

  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="text-base">Notification channels</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <p className="text-xs text-muted-foreground">
          A firing alert goes to every enabled channel whose minimum severity it reaches: once when it starts, again
          every 4 hours until someone acknowledges it, and once when it resolves. E-mail uses the SMTP relay from
          Settings; a webhook receives a JSON POST.
        </p>
        {channels && channels.length > 0 && (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>To</TableHead>
                <TableHead>From severity</TableHead>
                <TableHead className="text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {channels.map((c) => (
                <TableRow key={c.id} className={c.enabled ? "" : "opacity-60"}>
                  <TableCell className="text-sm">{c.name || (c.kind === "smtp" ? "E-mail" : "Webhook")}</TableCell>
                  <TableCell className="font-mono text-xs">{c.target_hint}</TableCell>
                  <TableCell>
                    <SeverityBadge severity={c.min_severity} />
                  </TableCell>
                  <TableCell>
                    <div className="flex justify-end gap-1.5">
                      <Button variant="outline" size="sm" disabled={!!busy} onClick={() => test(c)}>
                        {busy === `channel-test-${c.id}` ? (
                          <Loader2 className="mr-1 h-3.5 w-3.5 animate-spin" />
                        ) : (
                          <Send className="mr-1 h-3.5 w-3.5" />
                        )}
                        Test
                      </Button>
                      <Button
                        variant="outline"
                        size="sm"
                        disabled={!!busy}
                        onClick={() =>
                          run(`channel-toggle-${c.id}`, async () => {
                            await api.alerts.updateChannel(token!, c.id, { enabled: !c.enabled });
                            onChanged();
                          })
                        }
                      >
                        {c.enabled ? "Disable" : "Enable"}
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        disabled={!!busy}
                        aria-label="Delete channel"
                        onClick={() =>
                          run(`channel-delete-${c.id}`, async () => {
                            await api.alerts.deleteChannel(token!, c.id);
                            onChanged();
                          })
                        }
                      >
                        <Trash2 className="h-3.5 w-3.5" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        {channels && channels.length === 0 && (
          <p className="text-sm text-muted-foreground">No channel yet: alerts are recorded here but nobody is told.</p>
        )}

        <form onSubmit={add} className="grid items-end gap-3 md:grid-cols-[8rem_1fr_2fr_9rem_auto]">
          <div className="space-y-1">
            <Label className="text-xs">Kind</Label>
            <select
              className="flex h-9 w-full rounded-md border bg-transparent px-2 text-sm"
              value={kind}
              onChange={(e) => setKind(e.target.value as "smtp" | "webhook")}
            >
              <option value="smtp">E-mail</option>
              <option value="webhook">Webhook</option>
            </select>
          </div>
          <div className="space-y-1">
            <Label className="text-xs">Name</Label>
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="On-call" />
          </div>
          <div className="space-y-1">
            <Label className="text-xs">{kind === "smtp" ? "Address" : "URL"}</Label>
            <Input
              value={target}
              onChange={(e) => setTarget(e.target.value)}
              type={kind === "smtp" ? "email" : "url"}
              placeholder={kind === "smtp" ? "oncall@example.com" : "https://hooks.example.com/…"}
              autoComplete="off"
              required
            />
          </div>
          <div className="space-y-1">
            <Label className="text-xs">From severity</Label>
            <select
              className="flex h-9 w-full rounded-md border bg-transparent px-2 text-sm"
              value={minSeverity}
              onChange={(e) => setMinSeverity(e.target.value as AlertSeverity)}
            >
              <option value="info">info</option>
              <option value="warning">warning</option>
              <option value="critical">critical</option>
            </select>
          </div>
          <Button type="submit" disabled={!!busy || !target}>
            <Plus className="mr-1 h-4 w-4" />
            Add
          </Button>
        </form>
      </CardContent>
    </Card>
  );
}
