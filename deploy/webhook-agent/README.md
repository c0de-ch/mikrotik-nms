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
