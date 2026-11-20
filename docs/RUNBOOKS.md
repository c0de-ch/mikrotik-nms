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
