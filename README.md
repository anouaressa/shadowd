# shadowd

A "shadow copy"-style service for Linux: it takes deduplicated snapshots of
files on a cron schedule and lets you list or restore any previous version.
Zero third-party dependencies — builds with the Go standard library only.

## How it works

1. **Chunking** (`chunker/`) — each file is split into variable-sized,
   content-defined chunks using a windowed rolling hash (buzhash). A
   boundary is chosen based on the data itself, not a fixed offset, so
   inserting or deleting bytes only disturbs the chunk(s) touching that
   edit — every other chunk re-syncs to the same boundaries as before and
   hashes identically to the previous version.
2. **Storage** (`store/`) — each chunk is saved once, named by its SHA-256
   hash. Saving a chunk that's already on disk is a no-op. This is the
   entire deduplication mechanism: a snapshot only costs you the bytes that
   are genuinely new.
3. **Manifest** (`manifest/`) — a small JSON index recording, per tracked
   file, the ordered list of chunk hashes for every snapshot taken. This is
   what "a version" actually is: not a copy, just a list of pointers.
4. **Scheduling** (`cronsched/`) — a self-contained parser for standard
   5-field cron expressions (`minute hour dom month dow`), plus the
   `@every <duration>` shorthand.

## Build

```bash
go build -o shadowd .
```

## Configure

Create `shadowd.json`:

```json
{
  "store_dir": "/var/lib/shadowd/objects",
  "manifest_path": "/var/lib/shadowd/manifest.json",
  "schedule": "0 * * * *",
  "paths": [
    "/etc/nginx/nginx.conf",
    "/home/ess/notes/todo.md"
  ]
}
```

- `schedule` is a standard cron expression (`0 * * * *` = hourly) or
  `@every 30m` for simple intervals.
- `paths` lists individual files to track. (Directory trees aren't walked
  automatically in this first version — see "Extending" below.)

If a file hasn't changed since its last snapshot, no new version is
recorded, so a frequent schedule doesn't bloat the manifest with duplicate
entries for untouched files.

## Run

```bash
# One-shot: take a snapshot of every configured path right now
./shadowd snapshot -config shadowd.json

# Run forever on the configured cron schedule (also snapshots once on startup)
./shadowd daemon -config shadowd.json

# Same, plus serve the web dashboard on :8080
./shadowd daemon -config shadowd.json -listen :8080

# Web dashboard only, no cron loop (if something else already takes snapshots)
./shadowd serve -config shadowd.json -listen :8080

# See every recorded version of a file
./shadowd list -config shadowd.json /etc/nginx/nginx.conf

# Restore the latest version to a new path
./shadowd restore -config shadowd.json -out /tmp/nginx.conf.restored /etc/nginx/nginx.conf

# Restore the version as of a specific time
./shadowd restore -config shadowd.json \
  -out /tmp/nginx.conf.old \
  -at 2026-09-20T14:00:00Z \
  /etc/nginx/nginx.conf
```

Note: flags must come before the file path argument (standard Go `flag`
package behavior) — `restore -config c.json -out dest -at TIME /path/to/file`,
not the other way round.

## Web dashboard

Open `http://<host>:8080/` after starting with `-listen`. It shows:

- Every tracked file, sorted by most recently snapshotted, with version
  count and current size.
- Click a file to see its full version history — snapshot time, size, and
  how much *new* (non-deduplicated) data that version actually added.
- **Download** on any row reconstructs that exact version and streams it
  back as a file — it never touches the live file on disk.
- **Snapshot now**, shown only for paths listed in `config.json`, triggers
  an on-demand snapshot through the same code path the cron scheduler uses.
- The file list refreshes every 15 seconds, so snapshots taken by the
  background cron scheduler show up without a manual reload.

The whole frontend is a single embedded HTML file (`webui/static/index.html`)
compiled into the binary via `go:embed` — no separate assets to ship, no
CDN calls, works entirely offline. The API it talks to:

| Endpoint | Method | Purpose |
|---|---|---|
| `/api/config` | GET | Configured paths + schedule |
| `/api/files` | GET | Tracked files with version counts |
| `/api/versions?path=` | GET | Full version history of one file |
| `/api/restore?path=&index=` | GET | Download a specific version |
| `/api/snapshot?path=` | POST | Take a snapshot right now |

Restoring by `index` (the row's position in its history, 0 = oldest) is
deliberate rather than by timestamp: two snapshots can land in the same
second under a fast schedule or repeated manual triggers, which makes
"restore the version at time T" ambiguous between them. The UI always
knows the exact row it's asking for, so it asks for that directly.

There's no authentication on the dashboard — bind it to a private
interface (e.g. `-listen 127.0.0.1:8080` behind an SSH tunnel, or your
homelab's internal network) rather than exposing it publicly.

## Run as a systemd service

```ini
# /etc/systemd/system/shadowd.service
[Unit]
Description=shadowd file snapshot service
After=network.target

[Service]
ExecStart=/usr/local/bin/shadowd daemon -config /etc/shadowd.json
Restart=always
User=shadowd

[Install]
WantedBy=multi-user.target
```

```bash
sudo cp shadowd /usr/local/bin/
sudo systemctl daemon-reload
sudo systemctl enable --now shadowd
journalctl -u shadowd -f   # watch it snapshot on schedule
```

## Storage efficiency notes

- Average chunk size targets ~2 MiB (tunable via `maskBits` in
  `chunker/chunker.go` — lower it, e.g. to 18 for ~256 KiB average, if you
  want smaller "blast radius" per edit at the cost of more chunks/overhead
  per file).
- An edit changes only the chunk(s) it falls inside; everything else in the
  file dedupes against the previous snapshot automatically.
- There's no compression on top of this. If you want it, gzip each chunk
  before writing it in `store.Save` — content-defined chunking and
  compression are independent and stack fine.

## Extending

- **Directory trees instead of single files**: walk each configured path
  with `filepath.WalkDir` at snapshot time and call `Engine.Take` per file
  found; store versions keyed by the file's path as already implemented.
- **Garbage collection**: nothing currently deletes old chunks or old
  manifest entries, so this grows forever. A GC pass would enumerate every
  chunk hash referenced by every kept version, then delete any object in
  the store not in that set — straightforward, just not built yet since
  "which old versions to keep" is a policy decision (keep last N, keep
  daily-for-30-days-then-weekly, etc.) that's worth deciding deliberately
  rather than guessing.
- **Samba `vfs_shadow_copy2` integration**: if you want a Windows-style
  right-click "Previous Versions" tab, that module expects a directory of
  timestamp-named snapshot directories, not a chunk store — you'd add a
  `shadowd checkout <path> <timestamp> <dest-dir>` command that reconstructs
  a full directory tree at that point in time, laid out the way
  `vfs_shadow_copy2` expects.

