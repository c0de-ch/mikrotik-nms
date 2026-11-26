# Runbooks

What to check, and what to do, for each alert the NMS raises. Every alert
links to its section here (`runbook_url`).

Alerts are evaluated every 30 seconds from what the pollers already store —
an alert never costs a device a command. A firing alert is sent to the
notification channels once, again every 4 hours until someone acknowledges
it, and once more when it resolves. A silence stops the notifications, not
the alert.

## device_unreachable

**Fires when** the liveness check (a TCP connect to the device's API port
every 30 seconds) has failed for longer than `offline_threshold_seconds`
(Settings, default 120 s). Severity: critical. **Resolves** on the first
successful check.

Check, in this order:

1. **Is it one device or several?** Several devices behind the same switch
   going unreachable together point at their uplink, not at them. The map
   shows which links went down.
2. **Is the device up at all?** Ping it from another host on the management
   network; look at its upstream switch port (link state, PoE, errors) on the
   Network Health page.
3. **Up, but the API port does not answer?** The API service may be disabled
   or restricted (`/ip service print` — check the allowed addresses include
   the NMS), or a firewall rule changed. The device answers WinBox or SSH in
   that case.
4. **Did the NMS move?** If the NMS host changed its address, devices that
   restrict the API service to the old address refuse it — every device
   goes unreachable at once.

Planned work: silence the device (or pause its polling on the device page)
before you start.

## parallel_api_poller

**Fires when** a device's log shows an API login from an address that is not
this NMS, and a read of `/user/active` confirmed the session is live.
Severity: warning. **Resolves** about two hours after the last confirmed
login.

Something else is polling the device over the API. On small devices a second
poller can double the management CPU load.

1. The alert names the user and the source address. Find out what runs
   there: another monitoring tool, a script, a second (development or
   forgotten) instance of this NMS.
2. If it is this NMS behind NAT or with several addresses, add those
   addresses to the `nms_source_addresses` setting.
3. If it should not be polling: stop it at the source, then restrict the API
   service on the devices to the NMS address (`/ip service set api
   address=<nms>/32`) so it cannot come back.

## flow_export_on_switch

**Fires when** flow datagrams (NetFlow / IPFIX) arrive from the address of a
managed device that must not export flows: a switch, an access point or CPE,
or a device of the weakest CPU class (C1). Such a device forwards in
hardware, so the export shows almost nothing — and every exported packet
costs CPU it does not have.

Severity depends on what is known:

- **warning, marked "unconfirmed"** — only the UDP source address says so. An
  address can be forged, so this never becomes critical by itself.
- **critical** — the NMS read the device's own `/ip traffic-flow` settings
  (two read-only prints, at most once per hour per device, never while the
  device's polling is paused) and the device confirmed it: traffic-flow is
  enabled and has an enabled target. The alert then carries the rollback
  steps for the device's RouterOS version.

While datagrams keep arriving the read is repeated every hour. A
confirmation that could not be renewed for about two hours (the device is
paused or unreachable) no longer counts: the alert is a warning again until
the device can be read.

The NMS drops the datagrams (the sender is listed under Traffic → Flows →
Flow exporters → Blocked senders) — unless the device already was an
exporter before, which keeps working and is flagged in the exporter list.
The alert fires either way, also when such an exporter is set to "ignore
datagrams": the device exports regardless of what the NMS does with them.
**Resolves** about 30 minutes after the last datagram.

What to do:

1. **Unconfirmed?** Check by hand, read-only:
   `/ip traffic-flow print` and `/ip traffic-flow target print` on the device.
   - `enabled=no`, or no enabled target: the device does not export. Something
     else on the network sends datagrams with its address — find the real
     sender (a misconfigured exporter with a wrong `src-address`, a moved
     address, a host that forges it). The alert stays a warning.
   - It does export: go on with step 2. (The NMS confirms it by itself within
     the hour, unless the device is paused or offline.)
2. **Confirmed: stop the export on the device.** Use the rollback steps in
   the alert (also under Traffic → Flows → "How to stop the export", and in
   [FLOW-EXPORT.md](FLOW-EXPORT.md#rollback)). Keep their order:
   1. Run `/tool profile duration=30` first. If the `traffic-flow` rows are
      near 0 %, disable and remove only the target and leave `enabled=yes`
      until the next planned upgrade.
   2. Disable the target, then — only if still needed — set `enabled=no`.
      **Before RouterOS 7.25, disabling traffic-flow on a busy system can
      crash the kernel**: download the update first
      (`/system package update download`) and do it in a maintenance window
      with a console ready. Safe Mode does not protect against a kernel crash.
   3. Remove the target and verify with the profiler.
3. **Do not "fix" it in the NMS.** Ignoring or deleting the exporter, or
   blocking the sender, does not stop the device from exporting.
4. **It is a router after all?** The alert names the role the NMS derived and
   why (the device page shows it too). A router is recognised by an internet
   or VPN egress of its own; a device the NMS classes as a switch or an access
   point by its board stays one. If you decide to accept its flows anyway, an
   admin adds the sender with "Add anyway…" — the alert stays open as a
   reminder; acknowledge it to stop the notifications.

Want the traffic of a switch port? Add a derived view from the router or
firewall next to it instead (FLOW-EXPORT.md, "Switches that cannot export").
