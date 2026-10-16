# CI/CD & Container Images

How code gets from a pull request to a published container image and a running
install. Everything here lives in [`.github/workflows/`](../.github/workflows/)
and [`deploy/`](../deploy/); nothing needs repository secrets — every workflow
runs on the automatic `GITHUB_TOKEN`.

| Workflow | File | Runs on | Triggered by | Produces |
|---|---|---|---|---|
| **CI** | [`ci.yml`](../.github/workflows/ci.yml) | GitHub-hosted `ubuntu-latest` | every pull request; push to `main`/`master` | pass/fail status checks |
| **Build & Push Docker Images** | [`docker.yml`](../.github/workflows/docker.yml) | GitHub-hosted `ubuntu-latest` | push to `main`/`master`; tags `v*` | multi-arch images on GHCR |
| **Deploy to LXC** | [`deploy-lxc.yml`](../.github/workflows/deploy-lxc.yml) | **self-hosted** runner inside the target LXC | push to `main` (app/deploy paths only); manual dispatch | an updated native install |

```
   pull request ──► CI (backend · frontend)            ← must be green to merge
        │
        ▼ merge
      main ───────► CI
        ├─────────► Build & Push Docker Images ──► ghcr.io/<owner>/<repo>/{backend,frontend}:latest, :sha-<short>
        └─────────► Deploy to LXC (self-hosted runner → install.sh --skip-deps)

   git tag vX.Y.Z ─► Build & Push Docker Images ──► :vX.Y.Z, :vX.Y, :sha-<short>
```

---

## 1. CI (`ci.yml`)

Two independent jobs; both must pass before a PR can be merged.

**Backend (build · vet · test)** — working directory `backend/`

1. `actions/setup-go@v5` with `go-version-file: backend/go.mod` (the Go version is
   whatever `go.mod` declares; the module cache is keyed on `go.sum`).
2. `go vet ./...`
3. `go build ./...`
4. `go test -race ./...` — the whole backend suite under the race detector. Fuzz
   targets (e.g. `FuzzDecode` in `internal/flow/decode`) run their seed corpus
   here; the 60 s fuzzing session is a separate local target (`make fuzz-flow`).

**Frontend (build · lint)** — working directory `frontend/`

1. `actions/setup-node@v4`, Node 22, npm cache keyed on `package-lock.json`.
2. `npm ci`
3. `npm run build` (`NEXT_TELEMETRY_DISABLED=1`) — a full Next.js production
   build, so type errors and broken imports fail here.
4. `npm run lint` with `continue-on-error: true`: lint results are **annotated
   but do not fail the job** while pre-existing lint debt is worked off (see
   `IMPROVEMENTS.md`). Flip it to `false` once `npm run lint` is clean.

The workflow only needs `contents: read`.

### Hosted-runner caveats

- Hosted runners are small (2 vCPU) and the race detector makes tests ~2–10×
  slower. Tests that drive real sockets or goroutines must wait on **observed
  state** (counters, channels), never on fixed sleeps or on assumptions about
  how many retries happened — see `TestCollectorRunSocketAndGracefulShutdown`
  in `backend/internal/flow/collector_test.go` for the pattern.
- A `go test` failure that passed on the PR run but failed on the identical
  `main` push is a timing flake. Re-run it, then fix the test so it cannot
  recur:

  ```bash
  gh run list --workflow ci.yml --limit 5          # find the run id
  gh run view <id> --log-failed | grep -E -- '--- FAIL|_test.go:'
  gh run rerun <id> --failed                       # re-queue only the failed job
  ```

---

## 2. Container images (`docker.yml`)

### Triggers

- Push to `main` or `master` → images tagged `latest` + `sha-<short>`.
- Push of a tag matching `v*` → images tagged `vX.Y.Z`, `vX.Y`, `sha-<short>`
  (**not** `latest`, which only follows the default branch).
- Pull requests do **not** build images.
- There is no `workflow_dispatch`; to rebuild the current `main` re-run the last
  run (`gh run rerun <id>`), or push a tag.

Permissions: `contents: read`, `packages: write` — the `GITHUB_TOKEN` is used
to log in to `ghcr.io` (`username: ${{ github.actor }}`).

### Jobs

`build-backend` and `build-frontend` run in parallel; each does:

1. `actions/checkout@v4`
2. `docker/setup-qemu-action@v3` — registers binfmt handlers so the `arm64`
   half of the build can run on the `amd64` runner.
3. `docker/setup-buildx-action@v3`
4. `docker/login-action@v3` against `ghcr.io`
5. `docker/metadata-action@v5` — computes tags and OCI labels (below).
6. `docker/build-push-action@v6` — `platforms: linux/amd64,linux/arm64`,
   `push: true`, layer cache `type=gha,mode=max`.

| | Backend | Frontend |
|---|---|---|
| Dockerfile | [`deploy/docker/Dockerfile.backend`](../deploy/docker/Dockerfile.backend) | [`deploy/docker/Dockerfile.frontend`](../deploy/docker/Dockerfile.frontend) |
| Build context | `./backend` | `./frontend` |
| Builder | `golang:alpine`, `CGO_ENABLED=0 go build -ldflags="-s -w"` (`GOTOOLCHAIN=local`) | `node:22-alpine`, `npm ci && npm run build` (standalone output) |
| Runtime | `alpine:3.21` + `ca-certificates`, `tzdata`; `ENTRYPOINT mikrotik-nms` | `node:22-alpine`, `USER node`, `CMD node server.js` |
| Ports | `8080/tcp`, `2055/udp` (flow collector, off unless `MIKROTIK_NMS_FLOW_LISTEN` is set) | `3000/tcp` |
| Healthcheck | none in the image (Compose/K8s probe `/api/v1/health`) | `wget http://localhost:3000/login` every 30 s |
| Build args | — | `NEXT_PUBLIC_APP_VERSION=${{ github.ref_name }}`, `NEXT_PUBLIC_COMMIT_SHA=${{ github.sha }}` |

The two build args are **inlined into the JavaScript bundle** at build time and
shown in the sidebar footer (`v<version> · <sha>`): a `main` build shows
`vmain · aee1223`, a tag build `v0.7.0 · <sha>`. `NEXT_PUBLIC_API_URL` /
`NEXT_PUBLIC_WS_URL` are deliberately **not** set, so published frontend images
use the same-origin default and work behind any reverse proxy that serves
`/api/*` (see [Environment Variables](../README.md#environment-variables) in
the README). Never bake a URL into the shared image; build your own if you
need a split-origin deployment.

### Image names and tags

`docker/metadata-action` derives the name from the repository:

```
ghcr.io/${{ github.repository }}/backend     → ghcr.io/c0de-ch/mikrotik-nms/backend
ghcr.io/${{ github.repository }}/frontend    → ghcr.io/c0de-ch/mikrotik-nms/frontend
```

so a fork publishes under its own owner/repo path. The K8s manifests
(`deploy/k8s/deployment.yaml`) and the README reference the upstream path —
adjust them in a fork.

| Tag | When | Example |
|---|---|---|
| `latest` | every push to the default branch | `backend:latest` |
| `sha-<short sha>` | every push and every tag build | `backend:sha-aee1223` |
| `vX.Y.Z` | push of tag `vX.Y.Z` | `backend:v0.7.0` |
| `vX.Y` | push of tag `vX.Y.Z` | `backend:v0.7` |

Every tag is a **multi-arch manifest list** (`linux/amd64` + `linux/arm64`), so
`docker pull` on a Raspberry Pi, an ARM VM or a RouterOS container host gets
the right variant automatically. The action also stamps OCI labels
(`org.opencontainers.image.source`, `.revision`, `.version`, …); the `source`
label is what makes GHCR show the package on the repository page.

### Timing and cache

The arm64 halves run under QEMU emulation. Expect ~2–3 min for the backend and
**5–8 min for the frontend** (Next.js build emulated). Layers are cached in the
GitHub Actions cache (`type=gha`), which is scoped per repository and evicted
at 10 GB / after 7 days unused, so the first build after a quiet period is slow
again.

---

## 3. GHCR packages: who owns them, linking, visibility

This is the part that is easy to get wrong, and it bit this repository for
months (see [Troubleshooting](#7-troubleshooting)).

- A GHCR package is owned by the **account** (user or organisation), not by
  the repository. It is listed under `https://github.com/<owner>?tab=packages`
  and named `<repo>/backend`, `<repo>/frontend`.
- A package is **linked** to the repository whose workflow first pushed it.
  Linking is what grants that repository's `GITHUB_TOKEN` read/write access;
  it is also what shows the package in the repo sidebar. A `GITHUB_TOKEN` has
  **no access at all** to an existing package that is linked to a different
  repository (or to none), even with `packages: write` — the push fails with
  `permission_denied: read_package`.
- When a package is first created by a workflow of a **public** repository it
  comes up **public**; a private repo creates a private package. Visibility
  can be changed afterwards in *Package settings → Danger zone*.
- Packages are **never cleaned up automatically**. Each `main` push adds a
  `sha-` tag; each tag is an index plus one manifest per platform (plus
  attestation manifests), so the version count grows by ~6 per push. Prune
  old untagged/`sha-` versions by hand in the package UI or with
  `actions/delete-package-versions` if it becomes a problem.

### Inspecting packages

```bash
# Anonymous: what is published and for which platforms?
docker manifest inspect ghcr.io/c0de-ch/mikrotik-nms/backend:latest | grep -E 'architecture|os"'

# With the GitHub CLI (needs the read:packages scope once):
gh auth refresh -h github.com -s read:packages
gh api "user/packages?package_type=container" \
  -q '.[]|"\(.name)\t\(.visibility)\trepo=\(.repository.full_name // "UNLINKED")"'
gh api --paginate "user/packages/container/mikrotik-nms%2Fbackend/versions" \
  -q '.[]|"\(.created_at[0:10])\t\(.metadata.container.tags|join(","))"' | head
```

(For an organisation-owned repo use `orgs/<org>/packages…` instead of
`user/packages…`.)

---

## 4. Releases

A release is just a tag:

```bash
git checkout main && git pull
git tag -a v0.8.0 -m "v0.8.0"
git push origin v0.8.0
```

That triggers `docker.yml` with `github.ref_name = v0.8.0`, producing
`:v0.8.0`, `:v0.8` and `:sha-<short>` for both images, with the frontend
reporting `v0.8.0` in its footer. Tags do **not** run CI (only PRs and
default-branch pushes do), so tag a commit that is already green on `main`.
`latest` keeps tracking `main`, not the newest tag — pin `vX.Y` or `vX.Y.Z`
where you need reproducible deployments.

---

## 5. Using the published images

**Docker Compose.** [`docker-compose.yml`](../docker-compose.yml) builds both
images locally. To run the published ones instead, replace each `build:`
block with an `image:` line and pull:

```yaml
services:
  backend:
    image: ghcr.io/c0de-ch/mikrotik-nms/backend:latest   # or :v0.7, :sha-aee1223
  frontend:
    image: ghcr.io/c0de-ch/mikrotik-nms/frontend:latest
```

```bash
docker compose pull && docker compose up -d
```

**Kubernetes.** [`deploy/k8s/deployment.yaml`](../deploy/k8s/deployment.yaml)
references `…:latest` with `imagePullPolicy: Always`, so a new image is picked
up whenever a pod is (re)created — after a `main` build, run
`kubectl -n <ns> rollout restart deployment/mikrotik-nms-backend
deployment/mikrotik-nms-frontend` (or pin a version tag and change it per
release). See [DEPLOYMENT.md §4](DEPLOYMENT.md#4-kubernetes).

**RouterOS container.** The backend image is the one referenced by the
`/container/add remote-image=ghcr.io/c0de-ch/mikrotik-nms/backend:latest …`
recipe in the [README](../README.md#deploy-on-mikrotik-chr-container); the
arm64 variant is selected automatically on ARM routers. This only works while
the package is **public** (RouterOS pulls anonymously).

**A specific build.** Every commit on `main` has an image:
`docker pull ghcr.io/c0de-ch/mikrotik-nms/backend:sha-$(git rev-parse --short=7 <commit>)`.

> The native LXC/bare-metal install (`deploy/lxc/install.sh`) does **not** use
> these images — it builds from source on the host. The images are for Docker,
> Kubernetes and RouterOS container deployments.

---

## 6. Deploy to LXC (`deploy-lxc.yml`)

The optional continuous-deploy path for a native install. It is one of two
alternatives — the other is the HMAC webhook agent in
[`deploy/webhook-agent/`](../deploy/webhook-agent/), which needs no Actions
runner at all. Both are compared in
[DEPLOYMENT.md §6](DEPLOYMENT.md#6-continuous-deployment).

**Triggers**

- `workflow_dispatch` with an optional `ref` input (default `main`).
- `push` to `main`, but only when `backend/**`, `frontend/**`, `deploy/lxc/**`
  or the workflow file itself changed (docs-only commits do not deploy).
- The push trigger is skipped when the repository **variable**
  `DEPLOY_ON_PUSH` is `"false"` (Settings → Secrets and variables → Actions →
  Variables); manual dispatch still works.
- `concurrency: deploy-lxc`, `cancel-in-progress: false` — deploys queue
  behind each other and never interrupt a running one. `timeout-minutes: 30`.

**Where it runs:** `runs-on: [self-hosted, linux, mikrotik-nms]` — a runner
registered by [`deploy/lxc/runner-install.sh`](../deploy/lxc/runner-install.sh)
**inside the LXC that hosts the install**. GitHub never connects inbound; the
runner polls GitHub. No SSH keys or secrets are stored in the repository.

**Steps**

1. Checkout the requested ref (`fetch-depth: 1`).
2. Verify `/usr/local/sbin/mikrotik-nms-deploy` exists (installed by
   `runner-install.sh`; fails fast with a clear error otherwise).
3. `sudo -n /usr/local/sbin/mikrotik-nms-deploy "$GITHUB_WORKSPACE"` — the
   only command the runner user may run as root (narrow sudoers grant, no env
   passthrough). The wrapper ([`runner-deploy.sh`](../deploy/lxc/runner-deploy.sh))
   checks the workspace is under the runner root and contains
   `backend/go.mod`, `frontend/package.json` and the installer, then runs
   `install.sh --skip-deps --source <workspace>`, and finally asserts the
   systemd units are active and `/api/v1/health` returns 200.
4. `systemctl status` of `mikrotik-nms-backend`, `mikrotik-nms-frontend`,
   `caddy` (always, for the log).

**If no runner is registered** the run does not fail — it sits in *Queued /
Waiting for a runner* and GitHub cancels it automatically after roughly a day.
Either register the runner (`runner-install.sh`), or set `DEPLOY_ON_PUSH=false`
to stop the queued runs, or use the webhook-agent path instead.

**Security notes for a public repository:** the deploy job only triggers on
`push` to `main` and on dispatch, never on pull requests, so a PR cannot
reach the self-hosted runner. Still enable *Settings → Actions → "Require
approval for first-time contributors"* and keep the runner limited to this
repository (do not share it at the account level).

---

## 7. Troubleshooting

**`denied: permission_denied: read_package` (or `write_package`) when pushing to GHCR**

The package already exists under the owner account but is linked to another
repository — typically the original repo was deleted, renamed, re-created or
made private/public under a new name, and the packages stayed behind. (This
repository's `mikrotik-nms/backend` and `/frontend` packages were created by
an earlier private repository; every image push failed from 2026-05 until the
orphans were deleted on 2026-10-16.) The `GITHUB_TOKEN` cannot even read such a
package, so `packages: write` does not help.

1. Confirm: `gh api "user/packages?package_type=container"` (see §3) and look
   at `repository.full_name` for the two packages.
2. Fix, either way:
   - **Link** — package → *Package settings → Manage Actions access → Add
     repository* → this repo with role **Write** (keeps old versions), or
   - **Delete and recreate** — `gh auth refresh -s delete:packages`, then
     `gh api -X DELETE "user/packages/container/<repo>%2Fbackend"` (same for
     `frontend`), then re-run the Docker workflow. The new packages are linked
     to this repository and inherit its visibility.

**`installation not allowed to Create organization package`** — the
organisation disallows `GITHUB_TOKEN` package creation; an org admin must
allow it under *Organisation settings → Packages*, or create the package once
with a PAT and link it.

**Image pushed but `docker pull` is denied anonymously** — the package is
private. *Package settings → Danger zone → Change visibility → Public*.
Kubernetes can alternatively use an `imagePullSecret`; RouterOS cannot.

**Frontend image job is very slow or times out** — the arm64 Next.js build is
emulated. 5–8 min is normal; if the GHA cache was evicted it is slower. If it
ever exceeds the 6 h job limit, split the platforms into separate jobs or
build arm64 on a native ARM runner.

**CI green on the PR, red on `main` for the same commit** — a timing flake on
the slower hosted runner. Re-run the failed job (`gh run rerun <id> --failed`)
and fix the test to wait on observed state (see §1).

**Deploy to LXC stuck in "Queued"** — no self-hosted runner with the
`mikrotik-nms` label is online. Check *Settings → Actions → Runners*; on the
LXC: `systemctl status actions.runner.*`. See §6.

**Kubernetes still runs the old image** — `latest` moved but the pods did not
restart. `kubectl rollout restart deployment/…`, or pin a `vX.Y.Z` tag.

**Need to republish `latest` without a code change** — `gh run list
--workflow docker.yml --limit 1` then `gh run rerun <id>`.
