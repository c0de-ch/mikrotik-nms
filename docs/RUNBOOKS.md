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
