"use client";

import { useMemo, useState } from "react";
import { ArrowRight, ChevronDown, ChevronRight, ExternalLink, Radio, Search } from "lucide-react";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import {
  bandLabel,
  countBy,
  deriveRoams,
  eventBadge,
  formatDateTime,
  formatRate,
  isWireless,
  signalColor,
  signalQuality,
  timeAgo,
  type StatView,
  type WifiEntry,
  type WifiEvent,
} from "./lib";

interface DetailProps {
  current: WifiEntry[];
  history: WifiEntry[];
  liveEvents: WifiEvent[];
  resolveName: (mac: string, entryHostname?: string) => string;
  resolveIP: (mac: string, entryIP?: string) => string;
  onSelectClient: (mac: string) => void;
  onShowAP: (ap: string) => void;
}

const TITLES: Record<StatView, { title: string; description: string }> = {
  clients: { title: "Connected clients", description: "Everything associated right now, by network, band and signal quality." },
  aps: { title: "Access points", description: "Load and signal health per AP. Expand one to see its clients." },
  roams: { title: "Roaming", description: "Who moves between APs, and along which paths." },
  events: { title: "Live events", description: "Join, roam and leave events streamed since this page was opened." },
};

// WifiDetailSheet is the drill-down panel behind the stat cards.
export function WifiDetailSheet({ view, onClose, ...props }: DetailProps & { view: StatView | null; onClose: () => void }) {
  // Keep the last view mounted while the sheet animates closed.
  const [shown, setShown] = useState<StatView>("clients");
  if (view && view !== shown) setShown(view);
  const meta = TITLES[shown];

  return (
    <Sheet open={!!view} onOpenChange={(open) => !open && onClose()}>
      <SheetContent
        side="right"
        className="w-full gap-0 data-[side=right]:w-full data-[side=right]:sm:max-w-xl"
      >
        <SheetHeader className="border-b pr-12">
          <SheetTitle>{meta.title}</SheetTitle>
          <SheetDescription>{meta.description}</SheetDescription>
        </SheetHeader>
        <div className="flex-1 overflow-y-auto p-4">
          {shown === "clients" && <ClientsPanel {...props} />}
          {shown === "aps" && <APsPanel {...props} />}
          {shown === "roams" && <RoamsPanel {...props} />}
          {shown === "events" && <EventsPanel {...props} />}
        </div>
      </SheetContent>
    </Sheet>
  );
}

// ---------- shared bits ----------

function Chip({ active, onClick, children }: { active: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "rounded-full border px-2.5 py-0.5 text-xs transition-colors",
        active ? "border-primary bg-primary text-primary-foreground" : "hover:bg-muted"
      )}
    >
      {children}
    </button>
  );
}

function Section({ title, children, right }: { title: string; children: React.ReactNode; right?: React.ReactNode }) {
  return (
    <section className="space-y-2">
      <div className="flex items-center justify-between">
        <h3 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">{title}</h3>
        {right}
      </div>
      {children}
    </section>
  );
}

// QualityBar is a stacked good/fair/poor bar with a legend.
function QualityBar({ entries }: { entries: WifiEntry[] }) {
  const counts = { good: 0, fair: 0, poor: 0 };
  for (const e of entries) {
    const q = signalQuality(e.signal);
    if (q) counts[q]++;
  }
  const total = counts.good + counts.fair + counts.poor;
  if (total === 0) return null;
  const seg = [
    { key: "good", label: "Good > -60", cls: "bg-green-500", n: counts.good },
    { key: "fair", label: "Fair -60…-75", cls: "bg-yellow-500", n: counts.fair },
    { key: "poor", label: "Poor < -75", cls: "bg-red-500", n: counts.poor },
  ];
  return (
    <div className="space-y-1.5">
      <div className="flex h-2.5 overflow-hidden rounded-full bg-muted">
        {seg.map((s) => s.n > 0 && (
          <div key={s.key} className={s.cls} style={{ width: `${(s.n / total) * 100}%` }} title={`${s.label}: ${s.n}`} />
        ))}
      </div>
      <div className="flex gap-3 text-xs text-muted-foreground">
        {seg.map((s) => (
          <span key={s.key} className="flex items-center gap-1">
            <span className={cn("h-2 w-2 rounded-full", s.cls)} />
            {s.label.split(" ")[0]} <span className="font-medium text-foreground tabular-nums">{s.n}</span>
          </span>
        ))}
      </div>
    </div>
  );
}

function ClientRow({ e, name, ip, meta, onClick }: { e: WifiEntry; name: string; ip: string; meta: React.ReactNode; onClick: () => void }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="flex w-full items-center gap-3 rounded-md px-2 py-2 text-left text-xs transition-colors hover:bg-muted/60"
    >
      <div className="min-w-0 flex-1">
        <div className="truncate font-medium text-sm">{name || <span className="font-mono">{e.mac_address}</span>}</div>
        <div className="truncate text-muted-foreground">
          {name && <span className="font-mono">{e.mac_address}</span>}
          {ip && <span className="ml-2 font-mono">{ip}</span>}
          {meta && <span className="ml-2">{meta}</span>}
        </div>
      </div>
      {e.signal && <span className={cn("shrink-0 font-mono", signalColor(e.signal))}>{e.signal}</span>}
      <ChevronRight className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
    </button>
  );
}

function Empty({ children }: { children: React.ReactNode }) {
  return <p className="py-8 text-center text-sm text-muted-foreground">{children}</p>;
}

// ---------- Connected ----------

type ClientSort = "signal" | "name" | "ap";

function ClientsPanel({ current, resolveName, resolveIP, onSelectClient }: DetailProps) {
  const [ssid, setSsid] = useState<string | null>(null);
  const [band, setBand] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const [sort, setSort] = useState<ClientSort>("signal");
  const [showOther, setShowOther] = useState(false);

  const wireless = current.filter(isWireless);
  const other = current.filter((e) => !isWireless(e));
  const ssids = countBy(wireless, (e) => e.ssid || "—");
  const bands = countBy(wireless, (e) => bandLabel(e.band));

  const q = search.trim().toLowerCase();
  const rows = (showOther ? current : wireless)
    .filter((e) => !ssid || (e.ssid || "—") === ssid)
    .filter((e) => !band || bandLabel(e.band) === band)
    .filter((e) =>
      !q ||
      e.mac_address.toLowerCase().includes(q) ||
      resolveName(e.mac_address, e.host_name).toLowerCase().includes(q) ||
      resolveIP(e.mac_address, e.ip_address).includes(q) ||
      (e.ap_name || "").toLowerCase().includes(q)
    )
    .sort((a, b) => {
      if (sort === "signal") return (parseInt(a.signal) || 0) - (parseInt(b.signal) || 0);
      if (sort === "ap") return (a.ap_name || "").localeCompare(b.ap_name || "");
      const na = resolveName(a.mac_address, a.host_name) || a.mac_address;
      const nb = resolveName(b.mac_address, b.host_name) || b.mac_address;
      return na.localeCompare(nb);
    });

  return (
    <div className="space-y-5">
      <div className="grid grid-cols-3 gap-2 text-center">
        <MiniStat label="Wireless" value={wireless.length} />
        <MiniStat label="Networks" value={ssids.length} />
        <MiniStat label="Other / unknown" value={other.length} />
      </div>

      <Section title="Signal quality">
        <QualityBar entries={wireless} />
      </Section>

      <Section title="Filter">
        <div className="space-y-2">
          <div className="flex flex-wrap gap-1.5">
            <Chip active={!ssid} onClick={() => setSsid(null)}>All networks</Chip>
            {ssids.map(([s, n]) => (
              <Chip key={s} active={ssid === s} onClick={() => setSsid(ssid === s ? null : s)}>{s} · {n}</Chip>
            ))}
          </div>
          <div className="flex flex-wrap gap-1.5">
            <Chip active={!band} onClick={() => setBand(null)}>All bands</Chip>
            {bands.map(([b, n]) => (
              <Chip key={b} active={band === b} onClick={() => setBand(band === b ? null : b)}>{b} · {n}</Chip>
            ))}
          </div>
          <div className="flex items-center gap-2">
            <div className="relative flex-1">
              <Search className="absolute left-2.5 top-2 h-4 w-4 text-muted-foreground" />
              <Input placeholder="Name, MAC, IP, AP..." value={search} onChange={(e) => setSearch(e.target.value)} className="h-8 pl-8" />
            </div>
            <select
              aria-label="Sort clients"
              className="h-8 rounded-md border bg-transparent px-2 text-sm"
              value={sort}
              onChange={(e) => setSort(e.target.value as ClientSort)}
            >
              <option value="signal">Weakest first</option>
              <option value="name">Name</option>
              <option value="ap">AP</option>
            </select>
          </div>
          {other.length > 0 && (
            <label className="flex items-center gap-2 text-xs text-muted-foreground">
              <input type="checkbox" checked={showOther} onChange={(e) => setShowOther(e.target.checked)} />
              Include {other.length} non-wireless / unknown entr{other.length === 1 ? "y" : "ies"}
            </label>
          )}
        </div>
      </Section>

      <Section title={`Clients (${rows.length})`}>
        <div className="-mx-2">
          {rows.map((e) => (
            <ClientRow
              key={e.id}
              e={e}
              name={resolveName(e.mac_address, e.host_name)}
              ip={resolveIP(e.mac_address, e.ip_address)}
              meta={[e.ssid, e.ap_name, e.band && bandLabel(e.band)].filter(Boolean).join(" · ")}
              onClick={() => onSelectClient(e.mac_address)}
            />
          ))}
          {rows.length === 0 && <Empty>No clients match these filters.</Empty>}
        </div>
      </Section>
    </div>
  );
}

function MiniStat({ label, value }: { label: string; value: number | string }) {
  return (
    <div className="rounded-lg border p-2">
      <div className="text-lg font-semibold tabular-nums">{value}</div>
      <div className="text-[11px] text-muted-foreground">{label}</div>
    </div>
  );
}

// ---------- Access points ----------

function APsPanel({ current, history, resolveName, resolveIP, onSelectClient, onShowAP }: DetailProps) {
  const [open, setOpen] = useState<string | null>(null);
  const roamsIn = useMemo(() => {
    const m: Record<string, number> = {};
    for (const e of history) if (e.event === "roam" && e.ap_name) m[e.ap_name] = (m[e.ap_name] || 0) + 1;
    return m;
  }, [history]);

  const wireless = current.filter(isWireless);
  const byAP = new Map<string, WifiEntry[]>();
  for (const e of wireless) {
    const k = e.ap_name || "Unknown AP";
    byAP.set(k, [...(byAP.get(k) || []), e]);
  }
  const aps = [...byAP.entries()].sort((a, b) => b[1].length - a[1].length || a[0].localeCompare(b[0]));
  const max = aps[0]?.[1].length || 1;

  if (aps.length === 0) return <Empty>No access points with associated clients.</Empty>;

  return (
    <div className="space-y-2">
      {aps.map(([ap, clients]) => {
        const signals = clients.map((c) => parseInt(c.signal)).filter((v) => !isNaN(v));
        const avg = signals.length ? Math.round(signals.reduce((s, v) => s + v, 0) / signals.length) : null;
        const weak = signals.filter((v) => v <= -75).length;
        const expanded = open === ap;
        return (
          <div key={ap} className="rounded-lg border">
            <button
              type="button"
              onClick={() => setOpen(expanded ? null : ap)}
              aria-expanded={expanded}
              className="flex w-full items-start gap-3 p-3 text-left transition-colors hover:bg-muted/40"
            >
              {expanded ? <ChevronDown className="mt-0.5 h-4 w-4 shrink-0" /> : <ChevronRight className="mt-0.5 h-4 w-4 shrink-0" />}
              <div className="min-w-0 flex-1 space-y-1.5">
                <div className="flex items-center justify-between gap-2">
                  <span className="truncate font-medium">{ap}</span>
                  <span className="shrink-0 text-sm font-semibold tabular-nums">{clients.length}</span>
                </div>
                <div className="h-1.5 overflow-hidden rounded-full bg-muted">
                  <div className="h-full bg-primary" style={{ width: `${(clients.length / max) * 100}%` }} />
                </div>
                <div className="flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
                  {countBy(clients, (c) => c.ssid || "—").map(([s, n]) => (
                    <Badge key={s} variant="secondary" className="font-normal">{s} · {n}</Badge>
                  ))}
                  {avg !== null && <span>avg <span className={cn("font-mono", signalColor(String(avg)))}>{avg}</span></span>}
                  {weak > 0 && <span className="text-red-600">{weak} weak</span>}
                  {roamsIn[ap] ? <span>{roamsIn[ap]} roams in</span> : null}
                </div>
              </div>
            </button>
            {expanded && (
              <div className="border-t px-1 pb-2 pt-1">
                {[...clients]
                  .sort((a, b) => (parseInt(a.signal) || 0) - (parseInt(b.signal) || 0))
                  .map((c) => (
                    <ClientRow
                      key={c.id}
                      e={c}
                      name={resolveName(c.mac_address, c.host_name)}
                      ip={resolveIP(c.mac_address, c.ip_address)}
                      meta={[c.ssid, c.band && bandLabel(c.band), formatRate(c.tx_rate)].filter(Boolean).join(" · ")}
                      onClick={() => onSelectClient(c.mac_address)}
                    />
                  ))}
                <div className="px-2 pt-1">
                  <Button size="sm" variant="ghost" className="h-7 text-xs" onClick={() => onShowAP(ap)}>
                    <ExternalLink className="mr-1 h-3 w-3" /> Show on Current tab
                  </Button>
                </div>
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}

// ---------- Roaming ----------

function RoamsPanel({ history, resolveName, onSelectClient }: DetailProps) {
  const roams = useMemo(() => deriveRoams(history), [history]);
  const [limit, setLimit] = useState(30);

  if (roams.length === 0) return <Empty>No roaming events in the loaded history.</Empty>;

  const oldest = roams[roams.length - 1].entry.recorded_at;
  const roamers = countBy(roams, (r) => r.entry.mac_address).slice(0, 8);
  const paths = countBy(roams.filter((r) => r.from && r.from !== r.entry.ap_name), (r) => `${r.from}\u0000${r.entry.ap_name}`).slice(0, 8);
  const maxRoamer = roamers[0]?.[1] || 1;
  const clients = new Set(roams.map((r) => r.entry.mac_address)).size;

  return (
    <div className="space-y-5">
      <div className="grid grid-cols-3 gap-2 text-center">
        <MiniStat label="Roams" value={roams.length} />
        <MiniStat label="Clients" value={clients} />
        <MiniStat label="Since" value={timeAgo(oldest)} />
      </div>

      <Section title="Most active roamers">
        <div className="-mx-2">
          {roamers.map(([mac, n]) => {
            const name = resolveName(mac);
            return (
              <button
                key={mac}
                type="button"
                onClick={() => onSelectClient(mac)}
                className="flex w-full items-center gap-3 rounded-md px-2 py-1.5 text-left text-xs transition-colors hover:bg-muted/60"
              >
                <div className="w-40 min-w-0 shrink-0 truncate">
                  <span className="font-medium text-sm">{name || <span className="font-mono">{mac}</span>}</span>
                </div>
                <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-muted">
                  <div className={cn("h-full", n >= 10 ? "bg-red-500" : "bg-blue-500")} style={{ width: `${(n / maxRoamer) * 100}%` }} />
                </div>
                <span className="w-8 shrink-0 text-right font-mono tabular-nums">{n}</span>
              </button>
            );
          })}
        </div>
        {roamers.some(([, n]) => n >= 10) && (
          <p className="text-xs text-muted-foreground">
            Red bars: 10+ roams — often a client ping-ponging between two APs with similar signal.
          </p>
        )}
      </Section>

      {paths.length > 0 && (
        <Section title="Common paths">
          <div className="space-y-1">
            {paths.map(([key, n]) => {
              const [from, to] = key.split("\u0000");
              return (
                <div key={key} className="flex items-center gap-2 rounded-md border px-2.5 py-1.5 text-xs">
                  <span className="min-w-0 flex-1 truncate">{from}</span>
                  <ArrowRight className="h-3 w-3 shrink-0 text-muted-foreground" />
                  <span className="min-w-0 flex-1 truncate">{to}</span>
                  <Badge variant="secondary" className="shrink-0 tabular-nums">{n}</Badge>
                </div>
              );
            })}
          </div>
        </Section>
      )}

      <Section title="Recent roams">
        <div className="-mx-2">
          {roams.slice(0, limit).map(({ entry: e, from }) => {
            const name = resolveName(e.mac_address, e.host_name);
            return (
              <button
                key={e.id}
                type="button"
                onClick={() => onSelectClient(e.mac_address)}
                className="flex w-full items-center gap-3 rounded-md px-2 py-2 text-left text-xs transition-colors hover:bg-muted/60"
              >
                <div className="min-w-0 flex-1">
                  <div className="truncate font-medium text-sm">{name || <span className="font-mono">{e.mac_address}</span>}</div>
                  <div className="truncate text-muted-foreground">
                    {from || "?"} <ArrowRight className="inline h-3 w-3" /> {e.ap_name}
                  </div>
                </div>
                {e.signal && <span className={cn("shrink-0 font-mono", signalColor(e.signal))}>{e.signal}</span>}
                <span className="shrink-0 text-muted-foreground" title={formatDateTime(e.recorded_at)}>{timeAgo(e.recorded_at)}</span>
              </button>
            );
          })}
        </div>
        {roams.length > limit && (
          <Button size="sm" variant="outline" className="w-full" onClick={() => setLimit(limit + 50)}>
            Show more ({roams.length - limit} left)
          </Button>
        )}
      </Section>
    </div>
  );
}

// ---------- Live events ----------

function EventsPanel({ liveEvents, resolveName, onSelectClient }: DetailProps) {
  const [kind, setKind] = useState<string | null>(null);
  const counts = Object.fromEntries(countBy(liveEvents, (e) => e.event));
  const rows = kind ? liveEvents.filter((e) => e.event === kind) : liveEvents;

  if (liveEvents.length === 0) {
    return (
      <div className="py-10 text-center text-muted-foreground">
        <Radio className="mx-auto mb-3 h-8 w-8 animate-pulse" />
        <p className="text-sm">Listening for WiFi events…</p>
        <p className="mt-1 text-xs">They stream in while this page is open.</p>
      </div>
    );
  }

  return (
    <div className="space-y-5">
      <div className="grid grid-cols-3 gap-2">
        {(["join", "roam", "leave"] as const).map((k) => (
          <button
            key={k}
            type="button"
            onClick={() => setKind(kind === k ? null : k)}
            aria-pressed={kind === k}
            className={cn(
              "rounded-lg border p-2 text-center transition-colors hover:bg-muted/50",
              kind === k && "border-primary ring-1 ring-primary"
            )}
          >
            <div className="text-lg font-semibold tabular-nums">{counts[k] || 0}</div>
            <div className="text-[11px] capitalize text-muted-foreground">{k}s</div>
          </button>
        ))}
      </div>

      <Section
        title={kind ? `${kind} events (${rows.length})` : `All events (${rows.length})`}
        right={kind && <button type="button" className="text-xs text-primary" onClick={() => setKind(null)}>Clear filter</button>}
      >
        <div className="-mx-2">
          {rows.map((evt, i) => {
            const name = resolveName(evt.mac);
            return (
              <button
                key={`${evt.time}-${evt.mac}-${i}`}
                type="button"
                onClick={() => onSelectClient(evt.mac)}
                className="flex w-full items-center gap-3 rounded-md px-2 py-2 text-left text-xs transition-colors hover:bg-muted/60"
              >
                <span className="shrink-0">{eventBadge(evt.event)}</span>
                <div className="min-w-0 flex-1">
                  <div className="truncate font-medium text-sm">{name || <span className="font-mono">{evt.mac}</span>}</div>
                  <div className="truncate text-muted-foreground">
                    {evt.event === "roam" ? <>{evt.prev_ap} <ArrowRight className="inline h-3 w-3" /> {evt.ap}</> : evt.ap}
                  </div>
                </div>
                {evt.signal && <span className={cn("shrink-0 font-mono", signalColor(evt.signal))}>{evt.signal}</span>}
                <span className="shrink-0 text-muted-foreground">{timeAgo(evt.time)}</span>
              </button>
            );
          })}
        </div>
      </Section>
    </div>
  );
}
