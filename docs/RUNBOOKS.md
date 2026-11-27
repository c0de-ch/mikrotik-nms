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

## tls_pin_mismatch

**Fires when** a device set to API-TLS answered the TLS handshake with a
certificate whose SHA-256 fingerprint is not the one pinned for it. The NMS
aborted the handshake: **no user name or password was sent**. Severity:
critical. **Resolves** at once when the device is pinned to the certificate
it presents (or taken off TLS), otherwise 2 h 15 min after the last refused
handshake.

While it fires the device is not polled — only the liveness check reaches
it — and the pool retries the handshake about every ten minutes. The alert
and the device's Findings show the fingerprint that was **presented** and the
one that is **pinned**.

The alert cannot tell a renewed certificate from an impostor; the device
can:

1. **Do not paste the presented fingerprint from the alert into the device.**
   It is what answered at the device's address; whether that is the device
   is the question. Pinning it unchecked is trust on first use for whoever
   answered.
2. **Read the certificate on the device itself**, over a path that does not
   depend on that address: the serial console, MAC-WinBox / MAC-telnet from
   an adjacent device, or SSH if you have its host key on record.
   `/ip service print where name=api-ssl` names the certificate in use;
   `/certificate print detail where name=<that name>` prints `fingerprint=`.
3. **It equals the presented fingerprint:** the device's certificate changed
   — regenerated, renewed, the device reset or replaced and restored. Paste
   the fingerprint under the device page → **Connection**. The alert
   resolves and polling resumes within seconds.
4. **It equals the pinned fingerprint** — the device still has the pinned
   certificate, yet something else was presented: another host answers at
   the device's address. Find it: which MAC answers ARP for the address (the
   gateway's ARP table, the Clients page), behind which switch port (the
   port's "behind this port" panel). Common causes are a duplicate address or
   a replacement device brought up on the old address; a TLS-inspecting
   middlebox; and, on a flat management network, ARP spoofing. Treat the
   device's credentials as unexposed (nothing was sent) and the network as
   suspect until you know which.
5. **It matches neither:** you are looking at another certificate than the
   one api-ssl serves, or at another device. Recheck step 2.

The same refusal can come from **approving an IP auto-follow proposal** for
a TLS device: the certificate at the proposed address was not the device's,
so the move was refused — dismiss the proposal unless step 2 says otherwise.

Not an alert, but related: a TLS device with **no** pin at all is not logged
in to either. It shows **TLS pin missing** on the Devices page until an admin
enters the fingerprint (step 2 tells where to read it).
## Telemetry rules: what "unknown" means

`cpu_high`, `mem_low` and `port_egress_drops` judge the device's newest
recorded minutes (they lag the clock by one to a few minutes). A minute the
device was not measured in — paused, unreachable, no API session, the NMS
down — is **unknown, not healthy**:

- an alert opens only when **every** minute of its window was measured and
  bad;
- it resolves only once the window holds **no** bad minute and at least one
  measured one;
- in between (minutes that disagree, or nothing measured) nothing changes: an
  open alert stays open, none is opened.

So one of these alerts can outlive the device's reachability: if the device
then also shows `device_unreachable`, deal with that first — the telemetry
alert resolves by itself after the first calm, measured window, or when the
device is deleted.

## cpu_high

**Fires when** the device's CPU, sampled once a minute in a *quiet window*
(the NMS leaves the device alone for 3 s before it reads `cpu-load`, so its
own reads are not what is measured), stays at or above

- **80 %** on a weak (C1) device, **85 %** on a small or capable one (C2/C3),
  for **5 minutes** — warning;
- **95 %** for **10 minutes**, any class — critical.

**Resolves** after 5 minutes without a single minute at or above the warning
threshold.

1. **Is it us?** Open the device page → **CPU & NMS load**. Compare the
   quiet-window line with the post-poll markers (the CPU 3 s after an
   interface-counter read) and the NMS's commands and wall time per minute.
   High only around the markers, calm in between: the NMS's reads are the
   load — switch the device to the *light* profile or pause its polling on
   the same page, and check whether a live traffic view of it is open in some
   browser tab. High throughout, with a handful of commands a minute: it is
   not the NMS.
2. **Another poller?** A `parallel_api_poller` alert or finding on the same
   device means a second system polls it. SNMP polling and RouterOS
   `/tool graphing` do not show up there — check them on the device.
3. **The device's own work.** On the device: `/tool profile duration=10`
   names what uses the CPU (`bridging`, `firewall`, `management`, `wireless`
   …). `bridging` or `networking` on a switch whose traffic should be
   hardware-offloaded points at a port that lost `hw-offload`
   (`/interface bridge port print` on the device), a loop, or a broadcast
   storm (look for port-flap and loop events on the Network Health page at
   the same time).
4. **Since when?** The 7-day range of the same tab shows whether this began
   with a reboot (marked), a RouterOS upgrade, or a change in the network.

## mem_low

**Fires when** a device's free memory stays low for **3 minutes**: below
**5 %** of its total memory, or — on a weak (C1) device — below **8 MiB**.
Severity: warning. **Resolves** after 3 minutes with every reading above the
limit.

A RouterOS device that runs out of memory reboots (a watchdog or kernel
out-of-memory reset), usually without a useful log line.

1. **Trend or step?** Device page → **CPU & NMS load** → free memory over 24 h
   and 7 d. A slow decline over days that resets with every reboot marker is
   a leak — note the RouterOS version and look for a fixed release; a step
   down is something that was switched on.
2. **What holds the memory?** Typical on small devices: a large connection
   tracking or DNS cache table, a full routing table, many address-list
   entries, the `/tool graphing` and `/tool sniffer` buffers, logging to
   memory with a high line limit, packages that are installed but unused.
3. **Is it simply a small device?** Boards with 32 MiB of memory idle close
   to the 8 MiB floor. If its free memory is flat and it has run for weeks
   without a reboot marker, acknowledge the alert rather than chase it (an
   acknowledged alert is not notified again; a silence would cover all of
   the device's alerts, or this rule on every device).
4. Plan a reboot in a maintenance window if the decline continues; pause the
   device's polling first if it is already struggling.

## port_egress_drops

**Fires when** a port drops packets on the way out in each of **5
consecutive minutes**: at least **100 drops a minute** (`tx-drop` plus
`tx-queue-drop` of the interface) that are at least **0.1 %** of the packets
it sent in that minute. One alert per port. Severity: warning. **Resolves**
after 5 minutes in which no minute met both conditions.

Egress drops mean the port was offered more than it could send.

1. **Which port, and what is behind it?** Traffic page → the port → its
   history and the *behind this port* panel. A port running at its link speed
   is simply full: an uplink that needs more capacity, or one client
   saturating its access port (the flow view names the conversation where
   flow export covers the port).
2. **Negotiated speed and duplex.** A port that negotiated 100 Mbit/s or half
   duplex on a gigabit link drops under modest load: check the cable and the
   far end (`/interface ethernet monitor` on the device).
3. **A queue doing its job.** On a port with a queue or a shaper
   (`/queue simple`, `/queue tree`, an interface queue type with a small
   limit), drops are how the limit is enforced; a steady alert there says
   the limit is reached most of the time. Raise the limit, or acknowledge
   the alert if that is intended.
4. **Wireless and tunnel interfaces** drop when the medium or the path is
   slower than the offered load: look at the signal and rates of the clients
   behind the interface, or at the tunnel's underlay.

Interfaces that do not report these counters at all (some switch-chip ports)
never raise this alert.
