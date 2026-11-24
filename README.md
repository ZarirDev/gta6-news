# GTA6 Recomp blog → Discord monitor

A tiny Go program that watches `https://gta6recomp.com/blog` for new `a.post-row` entries and sends one Discord webhook embed per newly discovered post.

- Go standard library only; no third-party modules.
- First successful run creates a baseline and does **not** alert for existing posts.
- `monitor.json` stores safe configuration metadata and seen-post history.
- The actual Discord webhook URL is supplied through an environment variable and is **never written to JSON or committed**.
- In GitHub Actions, one run checks once and exits. Locally, it polls continuously unless `RUN_ONCE=1`.

## GitHub Actions setup (recommended)

1. Create a **public** GitHub repository (public standard GitHub-hosted Actions runs are currently free).
2. Add these files, preserving the `.github/workflows/monitor.yml` path:
   - `gta6recomp-monitor.go`
   - `.github/workflows/monitor.yml`
3. In the repository, open **Settings → Secrets and variables → Actions → New repository secret**.
   - Name: `DISCORD_WEBHOOK_URL`
   - Value: your complete Discord webhook URL.
4. Open **Settings → Actions → General → Workflow permissions**. The workflow declares `contents: write`; ensure repository policy allows it to push commits.
5. Push to the repository's default branch (`main` is typical).
6. Open **Actions → GTA6 Recomp blog monitor → Run workflow** once. The first run creates `monitor.json` with the current posts as a baseline and sends no alerts for them.
7. Later scheduled runs check for newly added posts every five minutes.

The workflow commits `monitor.json` only when it changes (first baseline or newly notified posts), not on every poll. This preserves seen-post history between disposable GitHub-hosted runners without a database or external storage.

## Configure it

Edit `.github/workflows/monitor.yml`:

- `WEBHOOK_NAME`: a label shown in the Discord embed footer and stored in `monitor.json`. It is **not** the secret webhook URL and does not rename the Discord webhook itself.
- `CHECK_INTERVAL`: informational metadata in Actions; the actual schedule is the cron expression.
- `cron: '2-59/5 * * * *'`: checks every five minutes. GitHub's minimum schedule interval is five minutes. Scheduled jobs can be delayed under load.

You can change the cron to a less frequent schedule, e.g. `*/10 * * * *` for every ten minutes.

## Run locally

Set the webhook secret and run:

```bash
# macOS / Linux
export DISCORD_WEBHOOK_URL='https://discord.com/api/webhooks/ID/TOKEN'
export CHECK_INTERVAL=5m
export WEBHOOK_NAME='GTA6 Recomp Blog Alerts'
go run ./gta6recomp-monitor.go
```

Windows PowerShell:

```powershell
$env:DISCORD_WEBHOOK_URL='https://discord.com/api/webhooks/ID/TOKEN'
$env:CHECK_INTERVAL='5m'
$env:WEBHOOK_NAME='GTA6 Recomp Blog Alerts'
go run .\gta6recomp-monitor.go
```

Build a standalone binary locally (optional):

```bash
go build -trimpath -ldflags='-s -w' -o monitor ./gta6recomp-monitor.go
```

GitHub Actions does **not** need a prebuilt binary: the workflow uses `go run`, which compiles the program on the runner. This avoids committing platform-specific binaries.

## State and reset

`monitor.json` contains the monitor URL, selector, check interval, webhook label, creation timestamp, initialization flag, seen post records, notification count, and last notification timestamp. It never contains the webhook URL/token.

To deliberately start over, delete `monitor.json` and run the workflow manually. The next run will establish a fresh baseline without sending alerts for posts already on the blog.

## Free-tier caveats

GitHub documents standard GitHub-hosted Actions runs in public repositories as free. However, no provider promises a service will remain free forever. GitHub scheduled workflows have a five-minute minimum interval, may be delayed, and scheduled workflows in public repositories are automatically disabled after 60 days without repository activity. Check the Actions tab occasionally and re-enable the workflow if GitHub disables it.
