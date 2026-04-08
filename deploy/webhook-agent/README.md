# MikroTik NMS — deploy webhook agent

A small Go HTTP daemon you run **inside your network**. It validates GitHub
webhook signatures and, on each accepted push, runs a configurable shell
command — typically a one-line SSH that pulls the latest code into the
mikrotik-nms LXC and re-runs `install.sh --skip-deps`.

The point: **the Proxmox API token, the LXC root SSH key, and any other
credentials never leave your network.** GitHub holds only an HMAC secret,
which is sufficient to authenticate the webhook but useless on its own.

```
┌──────────┐  signed POST   ┌─────────────────┐  ssh + git pull   ┌──────────┐
│  GitHub  │ ─────────────► │ webhook-agent   │ ────────────────► │ NMS LXC  │
│   push   │  HMAC-SHA256   │ (this daemon)   │  root@LXC         │ install  │
└──────────┘                └─────────────────┘                   └──────────┘
                            inside your network
                            holds: HMAC secret + SSH key
```

---

## What's in this directory

| File | Purpose |
|------|---------|
| `../../backend/cmd/deploy-agent/main.go` | The daemon (Go, single binary) |
| `install.sh` | Builds the binary, creates user/dirs, installs systemd unit |
| `agent.env.example` | Template for `/etc/mikrotik-nms-deploy-agent/env` |
| `run.sh.example` | Template `DEPLOY_COMMAND` — SSH + pull + install.sh |
| `systemd/mikrotik-nms-deploy-agent.service` | Hardened systemd unit |

---

## Threat model & security properties

### What an attacker needs to compromise this setup

| Capability | What's needed |
|---|---|
| Trigger an arbitrary deploy | Knowledge of the HMAC secret OR write access to the configured `ALLOWED_REPO` + `ALLOWED_REF` (i.e. push access to your repo) |
| Read the Proxmox API token | Local access to the agent host (the token isn't sent over the wire — it's only used by `run.sh` locally) |
| Read the LXC root SSH key | Local access to the agent host |
| Pivot from a compromised GitHub Actions / runner | Not possible — the agent never trusts inbound traffic from GitHub Actions runners; it only trusts signed payloads from the GitHub webhook delivery system itself |

### What the daemon does to harden the path

- **HMAC-SHA256 signature** verified with `crypto/subtle.ConstantTimeCompare` before any payload is parsed.
- **Body size capped** at 5 MiB.
- **Replay protection** via `X-GitHub-Delivery` ID cache (24 h TTL, 1000-entry soft cap).
- **Allow-listed `repository.full_name`** — push events for any other repo are dropped.
- **Allow-listed `ref`** — pushes to branches other than `ALLOWED_REF` are dropped (default: `refs/heads/main`).
- **Single deploy at a time** — a `sync.Mutex` serialises runs so two pushes in quick succession can't race.
- **Per-deploy timeout** — the `exec.Cmd` is killed if it runs longer than `DEPLOY_TIMEOUT` (default 30 min).
- **Listens on `127.0.0.1` by default** — designed to sit behind a reverse proxy or tunnel that terminates TLS.
- **Runs as a dedicated unprivileged user** with the full systemd hardening matrix (`ProtectSystem=strict`, `RestrictNamespaces`, etc.). The user has no shell.

### What the daemon **does not** do

- Doesn't fetch code itself — `run.sh` does that. So you can audit / change the deploy command without rebuilding the binary.
- Doesn't speak HTTPS directly — terminate TLS upstream (Tailscale Funnel, Cloudflare Tunnel, Caddy, nginx).
- Doesn't keep deploy history — logs go to systemd journal. Add a forwarder if you want them elsewhere.
- Doesn't allow on-demand triggers — only GitHub `push` events. This is intentional: it removes a whole class of "I can hit `/deploy` from a browser" mistakes.

### Public-repo specific notes

GitHub webhooks fire on push events from any source — including a malicious PR being merged by a compromised maintainer. Mitigations:

- **Branch protection** on `main` — require PR review + status checks before merge. The webhook only fires after the merge commit lands on `main`, so review is the gate.
- **Pin `ALLOWED_REF`** to exactly one branch (default `refs/heads/main`). The agent ignores everything else, so a force-push to `feat/*` or a tag push doesn't trigger anything.
- **Rotate the HMAC secret** if you ever suspect compromise. Edit `/etc/mikrotik-nms-deploy-agent/env`, restart the agent, update the value in GitHub.

---

## Where to run the agent

You have three reasonable choices, in order of how hard each is to set up:

| Host | Pros | Cons |
|---|---|---|
| **The Proxmox host itself** | Simplest. Direct LXC API access via local socket. No extra container. | Adds a network-listening process to the hypervisor. |
| **A dedicated tiny LXC** (256 MB / 1 vCPU / 2 GB disk) | Isolation from Proxmox host. Easy to snapshot/destroy. | Extra container to maintain. |
| **The mikrotik-nms LXC itself** | Zero extra hosts. | The thing being deployed and the thing doing the deploying are the same — restarting the backend during deploy is fine but tightens the coupling. |

The systemd unit and install script are agnostic — they work on any of the above. The recommended setup is **the dedicated LXC**, because it lets you reboot/snapshot the NMS LXC without affecting deploy capability, and lets you reboot the Proxmox host without breaking either.

---

## Setup

These instructions assume you've cloned this repo on the agent host (it can be a fresh Debian/Ubuntu box).

### 1. Run the installer

```sh
sudo ./deploy/webhook-agent/install.sh
```

The installer:

1. Installs Go 1.25.x to `/usr/local/go` (skip with `--skip-go` if already installed).
2. Builds `backend/cmd/deploy-agent` to `/opt/mikrotik-nms-deploy-agent/bin/deploy-agent`.
3. Creates the system user `mikrotik-nms-agent`.
4. Drops a templated env file to `/etc/mikrotik-nms-deploy-agent/env` with a randomly-generated `WEBHOOK_SECRET`.
5. Drops a sample deploy script to `/etc/mikrotik-nms-deploy-agent/run.sh`.
6. Installs and enables the systemd unit (but does not start it — you have to edit the configs first).

### 2. Edit the env file

```sh
sudo $EDITOR /etc/mikrotik-nms-deploy-agent/env
```

Set:

- `ALLOWED_REPO` to your GitHub `owner/repo` (e.g. `c0de-ch/mikrotik-c0de`)
- Note the auto-generated `WEBHOOK_SECRET` — you'll paste this into GitHub later
- Optionally tighten `LISTEN` (default `127.0.0.1:9000` is correct if you're using a reverse proxy or tunnel on the same host)

### 3. Edit the deploy script

```sh
sudo $EDITOR /etc/mikrotik-nms-deploy-agent/run.sh
```

Set:

- `LXC_HOST` to the IP or hostname of your mikrotik-nms LXC
- `BRANCH` if you don't deploy from `main`

### 4. Set up an SSH key for the agent → LXC connection

```sh
sudo -u mikrotik-nms-agent ssh-keygen -t ed25519 -N '' \
    -f /etc/mikrotik-nms-deploy-agent/id_ed25519
sudo cat /etc/mikrotik-nms-deploy-agent/id_ed25519.pub
```

Copy the public key into `/root/.ssh/authorized_keys` on the mikrotik-nms LXC. The simplest path:

```sh
# from any machine with SSH access to the LXC:
ssh root@<lxc-ip> "echo 'ssh-ed25519 AAAA...' >> /root/.ssh/authorized_keys"
```

> ⚠️ The agent runs as `mikrotik-nms-agent`, so the key file must be owned by that user and mode 0600. The `install.sh` already prepared `/etc/mikrotik-nms-deploy-agent/` with the right ownership for new files dropped in by `mikrotik-nms-agent` via `sudo -u`.

### 5. Start the agent

```sh
sudo systemctl start mikrotik-nms-deploy-agent
sudo systemctl status mikrotik-nms-deploy-agent
sudo journalctl -u mikrotik-nms-deploy-agent -f
```

You should see something like:

```
deploy-agent listening on 127.0.0.1:9000
  allowed repo: c0de-ch/mikrotik-c0de
  allowed ref:  refs/heads/main
  deploy timeout: 30m0s
```

### 6. Expose `/webhook` to GitHub

GitHub needs to be able to reach the agent. The agent listens on `127.0.0.1:9000` by default; you need a reverse-proxy or tunnel that takes inbound HTTPS traffic and forwards it to that local port. Pick whichever fits your setup:

#### Option A — Tailscale Funnel (recommended, easiest)

1. Install Tailscale on the agent host:
   ```sh
   curl -fsSL https://tailscale.com/install.sh | sh
   sudo tailscale up
   ```
2. Enable Funnel for the agent's port:
   ```sh
   sudo tailscale funnel --bg --https=443 127.0.0.1:9000
   ```
   This serves your agent at `https://<host>.<tailnet>.ts.net/` from the public internet, terminating TLS at Tailscale's edge.
3. Webhook URL for GitHub: `https://<host>.<tailnet>.ts.net/webhook`

No port-forwarding, no DNS, no certs. Tailscale Funnel is free for personal use and the relay can't decrypt the payload (it's TLS-terminated at your node).

#### Option B — Cloudflare Tunnel

1. Install `cloudflared` on the agent host.
2. `cloudflared tunnel login`, create a tunnel, and route a subdomain you own (e.g. `nms-deploy.example.com`) to `http://127.0.0.1:9000`.
3. Webhook URL for GitHub: `https://nms-deploy.example.com/webhook`

Free for any traffic volume that matters here. Requires owning a domain on Cloudflare.

#### Option C — Caddy + port-forward

If you already have a public IP and a domain pointed at your network:

1. Install Caddy on the agent host.
2. `/etc/caddy/Caddyfile`:
   ```
   nms-deploy.example.com {
       reverse_proxy 127.0.0.1:9000
   }
   ```
3. Forward port 443 (and 80 for ACME) from your router to the agent host.
4. Webhook URL for GitHub: `https://nms-deploy.example.com/webhook`

Caddy handles cert provisioning automatically.

#### Option D — WireGuard tunnel to a public-IP host (self-hosted relay)

For users who want zero third-party trust and don't want to expose the
agent host directly to the internet. The trade-off is that you need **one
host with a public IP** that you control — a cheap VPS (€3/mo Hetzner /
Netcup), an OpenWrt router, a homelab gateway, anything always-on with
port 443 reachable.

The setup is essentially "Tailscale Funnel, but self-hosted". GitHub talks
HTTPS to the public-IP host; that host runs Caddy + WireGuard and proxies
the request through an encrypted tunnel to the agent host inside your LAN.

```
GitHub ──HTTPS──► [VPS / public-IP host]  ──WireGuard──► [agent host (private)]
                  Caddy :443                              deploy-agent :9000
                  WG server                               WG client
```

##### 1. Install WireGuard on both hosts

```sh
# Debian/Ubuntu, both sides
sudo apt-get install -y wireguard
```

##### 2. Generate keys on each side

```sh
# on the VPS
wg genkey | tee /etc/wireguard/server.key | wg pubkey > /etc/wireguard/server.pub
chmod 600 /etc/wireguard/server.key

# on the agent host
wg genkey | tee /etc/wireguard/agent.key | wg pubkey > /etc/wireguard/agent.pub
chmod 600 /etc/wireguard/agent.key
```

Then exchange the `.pub` files between the two hosts (they're not secret).

##### 3. WireGuard server on the VPS

`/etc/wireguard/wg0.conf`:

```ini
[Interface]
Address = 10.88.0.1/24
ListenPort = 51820
PrivateKey = <contents of /etc/wireguard/server.key>

[Peer]
# the agent host
PublicKey = <contents of agent.pub>
AllowedIPs = 10.88.0.2/32
PersistentKeepalive = 25
```

```sh
sudo systemctl enable --now wg-quick@wg0
```

Open UDP/51820 on the VPS firewall (and only that port, plus 443 for the
GitHub-facing Caddy).

##### 4. WireGuard client on the agent host

`/etc/wireguard/wg0.conf`:

```ini
[Interface]
Address = 10.88.0.2/24
PrivateKey = <contents of /etc/wireguard/agent.key>

[Peer]
# the VPS
PublicKey = <contents of server.pub>
Endpoint = vps.example.com:51820
AllowedIPs = 10.88.0.1/32
PersistentKeepalive = 25
```

```sh
sudo systemctl enable --now wg-quick@wg0
```

Verify the tunnel:

```sh
# from the agent host
ping -c 3 10.88.0.1

# from the VPS
ping -c 3 10.88.0.2
curl -s http://10.88.0.2:9000/healthz   # should reach the deploy-agent
```

##### 5. Caddy on the VPS

`/etc/caddy/Caddyfile`:

```
nms-deploy.example.com {
    reverse_proxy 10.88.0.2:9000 {
        # only forward to the WG peer, never to localhost
        header_up Host {host}
    }
    log {
        output stdout
        format console
    }
}
```

```sh
sudo systemctl reload caddy
```

DNS-wise, point `nms-deploy.example.com` (an A record) at the VPS public
IP. Caddy provisions the cert via Let's Encrypt automatically on first hit.

##### 6. Lock the agent down to the WG interface

By default the agent listens on `127.0.0.1:9000`, which means only
processes on the agent host itself can talk to it — including the
WireGuard tunnel endpoint, which terminates locally. **The default is
already correct**: the WG peer's traffic enters the agent host's network
stack as if it were local, and `127.0.0.1` accepts it via the loopback
route through `wg0`.

If you'd rather bind the agent explicitly to the WireGuard address for
clarity, edit `/etc/mikrotik-nms-deploy-agent/env`:

```
LISTEN=10.88.0.2:9000
```

Then `systemctl restart mikrotik-nms-deploy-agent`. This makes the intent
explicit and prevents the agent from being accessible if WireGuard is
ever down.

##### 7. Webhook URL for GitHub

```
https://nms-deploy.example.com/webhook
```

##### Security notes for this setup

- **GitHub never reaches the agent host directly.** The only inbound port
  the agent host needs open is UDP/51820 from the VPS public IP — and
  even that you can drop by using a `PersistentKeepalive` and letting the
  agent host be the dialing peer, never the listener (the config above
  already does this).
- **If the VPS is compromised**, the attacker can hit
  `http://10.88.0.2:9000/webhook` directly, bypassing TLS — but they
  still need the `WEBHOOK_SECRET` to sign a valid request, and the agent
  rejects everything else with `401`. So a VPS compromise downgrades the
  threat to "knows your HMAC secret = can trigger deploys", which is the
  same threat surface as any other exposure option.
- **Rotate the WG keys** by regenerating on both sides and updating the
  configs. No agent restart needed — only `systemctl restart wg-quick@wg0`.
- **Lock the VPS Caddy down** so only `/webhook` and `/healthz` are
  reachable, by adding a `handle` block:
  ```
  handle /webhook* { reverse_proxy 10.88.0.2:9000 }
  handle /healthz  { reverse_proxy 10.88.0.2:9000 }
  handle           { respond 404 }
  ```
- The agent host doesn't need *any* inbound port open from the public
  internet. Only outbound UDP/51820 to the VPS.

### 7. Add the webhook in GitHub

In your repo on GitHub: **Settings → Webhooks → Add webhook**

| Field | Value |
|---|---|
| Payload URL | `https://<your-public-url>/webhook` |
| Content type | `application/json` |
| Secret | the value of `WEBHOOK_SECRET` from `/etc/mikrotik-nms-deploy-agent/env` |
| SSL verification | enabled |
| Which events | "Just the push event" |
| Active | yes |

GitHub sends a `ping` event on save — the agent answers `pong`. If you see a green checkmark on the webhook page, you're done.

### 8. Test it

Push something to `main`:

```sh
git commit --allow-empty -m "test deploy webhook"
git push origin main
```

In `journalctl -u mikrotik-nms-deploy-agent -f` you should see:

```
queueing deploy: c0de-ch/mikrotik-c0de @ a1b2c3d4e5f6 — test deploy webhook
[deploy] starting commit=a1b2c3d4e5f6
[deploy] ==> deploying a1b2c3d4e5f6 (test deploy webhook) to mikrotik-nms.lan
[deploy] ==> deploy OK
[deploy] OK in 1m12s
```

---

## Operations

```sh
# tail logs
journalctl -u mikrotik-nms-deploy-agent -f

# restart after editing the env or run.sh
systemctl restart mikrotik-nms-deploy-agent

# check status (also reachable from inside the host as http://127.0.0.1:9000/healthz)
curl -s http://127.0.0.1:9000/healthz | jq
```

## Updating the agent itself

```sh
cd /path/to/mikrotik-c0de
git pull
sudo ./deploy/webhook-agent/install.sh --skip-go
sudo systemctl restart mikrotik-nms-deploy-agent
```

The installer is idempotent and won't touch your env file or run.sh.

## Uninstall

```sh
sudo systemctl disable --now mikrotik-nms-deploy-agent
sudo rm /etc/systemd/system/mikrotik-nms-deploy-agent.service
sudo systemctl daemon-reload
sudo rm -rf /opt/mikrotik-nms-deploy-agent /var/lib/mikrotik-nms-deploy-agent /etc/mikrotik-nms-deploy-agent
sudo userdel mikrotik-nms-agent
```
