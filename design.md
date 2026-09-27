# shadowd Design

## Overview

shadowd is a lightweight file-versioning system inspired by shadow-copy backups. It monitors a configured set of files, captures periodic snapshots, and allows users to restore any previous version from the CLI or the embedded web UI.

The design emphasizes:

- content-defined chunking
- deduplicated storage
- manifest-based version tracking
- low dependency footprint
- reliable, crash-safe writes
- single-file web dashboard with no external frontend build step

This project intentionally uses only the Go standard library.

---

## Short System Design

### High-level diagram

```text
+---------------------+
| Config (JSON)       |
| - store_dir         |
| - manifest_path     |
| - schedule          |
| - paths[]           |
+----------+----------+
           |
           v
+---------------------+
| Snapshot Engine     |
| - read file         |
| - chunk file        |
| - dedupe chunks     |
| - record manifest   |
+----------+----------+
           |
           +---------------------+---------------------+
           |                     |                     |
           v                     v                     v
+------------------+   +------------------+   +------------------+
| Chunker          |   | Object Store     |   | Manifest         |
| rolling hash     |   | SHA-256 chunks   |   | version history  |
| variable chunks  |   | deduplicated     |   | path -> versions |
+------------------+   +------------------+   +------------------+
           |
           v
+--------------------------+
| Restore Engine           |
| load chunks by hash      |
| rebuild file from chunks |
| write to destination     |
+--------------------------+
```

### Quick “how a snapshot is created” flow

1. Load config and target file path.
2. Open the file and split it into content-defined chunks.
3. For each chunk, compute its SHA-256 hash.
4. Save only chunks not already present in the object store.
5. Compare the new chunk list with the last snapshot for that file.
6. If unchanged, skip writing a new manifest entry.
7. If changed, append a new version record with timestamp, chunk list, size, and new data count.
8. The file history is now available for list / restore / rollback.

---

## Core Goals

1. Store file history without copying entire files on every snapshot.
2. Detect unchanged files and avoid creating duplicate manifest entries.
3. Restore old versions quickly and reliably.
4. Keep implementation simple, robust, and easy to reason about.
5. Support both CLI usage and a browser dashboard.

---

## High-Level Architecture

The system is split into several small packages:

- chunker: splits files into variable-sized chunks
- store: content-addressed object storage for chunks
- manifest: version history index for each file
- snapshot: engine that takes and restores snapshots
- cronsched: cron or interval scheduling
- config: JSON configuration loader
- webui: embedded dashboard and HTTP API

The major flow is:

1. read configured file
2. split it into content chunks
3. save new chunks to object store
4. record version metadata in manifest
5. optionally expose via cron / web UI
6. restore by loading referenced chunks and writing them back in order

---

## 1. Chunking Strategy

### Why chunking matters
A naive backup system would store full copies of a file for every version. That is expensive and redundant.

Instead, shadowd uses content-defined chunking: the file is partitioned into chunks based on the content itself rather than fixed offsets.

### Rolling hash approach
The chunker uses a rolling hash (Buzhash-style windowed hash) so that when bytes are inserted or deleted, only the chunks near that edit are affected. The rest of the file retains the same chunk boundaries and therefore the same hashes.

This is essential for deduplication because the same unchanged content yields the same chunk signatures across snapshots.

### Why this works well for file versions
If the file changes a little:

- local edit area changes a small number of chunks
- most chunks remain identical
- those unchanged chunks are reused from prior snapshots
- only new content is stored

This dramatically reduces storage cost compared to full-file copies.

---

## 2. Deduplicated Storage

### Object store model
The object store stores each chunk once. The key is the SHA-256 hash of the chunk data.

A chunk is saved as:

- root/
  - first 2 hex chars of hash/
  - remaining hash as filename

This keeps the directory tree manageable and avoids a single huge flat directory.

### Deduplication logic
When a new chunk is created:

- compute its hash
- check whether that object already exists in the store
- if yes: do nothing
- if no: write it atomically

This means identical chunk content is stored exactly once, regardless of how many snapshots reference it.

### Storage efficiency
The project is designed so that a snapshot only costs the bytes that are truly new. Unchanged portions are reused by hash reference instead of duplicated on disk.

In practice, a file version is not “a copy of the file”; it is a list of chunk references.

---

## 3. Manifest Structure

The manifest is the version index. It records every snapshot of every tracked file.

Each manifest entry contains:

- timestamp: Unix epoch seconds
- chunks: ordered list of chunk hashes that reconstruct the file
- size: original file size
- new_bytes: how much newly-written data this version introduced

The manifest is a JSON file keyed by absolute file path:

- path -> list of version records

This keeps the metadata small while still allowing efficient listing and restoration.

### Ordering and ambiguity
Versions are stored oldest-first and sorted stably. This matters because multiple snapshots can land in the same second. When that happens, time-based restore can become ambiguous. The code therefore prefers index-based restore when a caller already knows which row it selected in the UI.

---

## 4. Snapshot Engine

The snapshot engine is the core orchestration layer.

### Take a snapshot
The Engine.Take flow is:

1. normalize the file path to an absolute path
2. open the file and read its content
3. split it into chunks
4. for each chunk:
   - compute hash
   - save chunk to store if not already present
   - count newly written bytes
5. compare against the latest manifest version
6. if content is identical, skip recording a new version
7. otherwise add a new manifest entry with the new chunk list and timestamp

### Duplicate suppression
If the latest recorded version has the same chunk sequence, the file is considered unchanged and no new snapshot is recorded.

This prevents a cron schedule from filling the manifest with repeated identical versions.

---

## 5. Restore Process

There are two restore paths:

### CLI restore by time
The CLI restore command accepts a timestamp and finds the latest snapshot at or before that time.

This is useful for human commands when a time is the natural input.

### Index-based restore
The engine also supports a restore-by-index operation. This is unambiguous and preferred by the web UI because the UI knows exactly which row the user selected.

The restore algorithm:

1. resolve the manifest version for the file
2. iterate over the chunk hashes in order
3. load each chunk from the object store
4. write all bytes back to the destination file

This reconstructs the file exactly as it existed at that snapshot.

---

## 6. Scheduling

The scheduler package handles cron expressions and shorthand intervals like:

- 0 * * * *
- @every 30m

The daemon mode runs a snapshot immediately on startup and then follows the schedule afterward. This is intentional: a user should see activity right away even if the configured cron interval is long.

---

## 7. Config and Runtime Model

The project uses a JSON config file with fields such as:

- store_dir
- manifest_path
- schedule
- paths

This file tells shadowd which files to track and where to keep the object store and manifest.

The config is intentionally simple and dependency-free.

---

## 8. Crash Safety and Atomicity

### Object store writes
The store writes chunks to a temporary file in the same directory and then renames it into place.

This reduces the risk of leaving a partially-written object behind under the final hash name.

### Manifest writes
The manifest is also written atomically: it is serialized to JSON, written to a temporary file, and renamed over the real manifest.

This avoids partial JSON writes that would corrupt version history.

---

## 9. Web Dashboard Design

The dashboard is a single embedded HTML page served by Go. It is embedded at compile time using Go’s embed package.

### Features
- list all tracked files
- show version count and latest file size
- show full version history with timestamps and new-data stats
- download any historical version
- manually snapshot a path
- rollback a live file to an older version

### API endpoints
The UI talks to the backend through a few JSON/HTTP endpoints:

- GET /api/config
- GET /api/files
- GET /api/versions?path=
- GET /api/restore?path=&index=
- POST /api/snapshot?path=
- POST /api/rollback?path=&index=

This keeps the frontend simple while reusing the same snapshot and restore engine logic as the CLI.

---

## 10. Why This Is a Shadow Copy System

A true shadow copy approach preserves previous versions without maintaining a full file copy for each revision.

In this design:

- the current file stays live on disk
- older versions live as reconstructed content from stored chunks
- manifest metadata tells the system which chunks belong to each snapshot
- restore can materialize any prior version on demand

This gives the user the experience of versioned file history without paying the storage cost of full duplication.

---

## 11. Setup, Configuration, and Usage Guide

### Build

From the project root:

```bash
go build -o shadowd .
```

This produces the executable used for all CLI commands.

### Config file

Create a file such as `shadowd.json`:

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

#### Fields

- `store_dir`: directory where deduplicated chunk objects are stored
- `manifest_path`: path to the JSON manifest that records all versions
- `schedule`: cron expression or interval like `@every 30m`
- `paths`: list of files to track

### Run the service in daemon mode

```bash
./shadowd daemon -config shadowd.json
```

This will:

- load the config
- create the object store if needed
- take an initial snapshot immediately
- keep running according to the configured schedule

### Run with the web UI

```bash
./shadowd daemon -config shadowd.json -listen 127.0.0.1:8080
```

Then open:

```text
http://127.0.0.1:8080/
```

### Web UI only

```bash
./shadowd serve -config shadowd.json -listen 127.0.0.1:8080
```

This mode runs only the UI and does not run the scheduler.

### Take one snapshot immediately

```bash
./shadowd snapshot -config shadowd.json
```

This snapshots every configured file once and exits.

### List versions of a file

```bash
./shadowd list -config shadowd.json /etc/nginx/nginx.conf
```

### Restore a file to a new path

```bash
./shadowd restore -config shadowd.json \
  -out /tmp/nginx.conf.restored \
  /etc/nginx/nginx.conf
```

### Restore a file as of a specific time

```bash
./shadowd restore -config shadowd.json \
  -out /tmp/nginx.conf.old \
  -at 2026-09-20T14:00:00Z \
  /etc/nginx/nginx.conf
```

### Use the dashboard

In the UI you can:

- select a tracked file
- view version history
- download a historical version
- trigger a snapshot immediately
- rollback a file to an earlier version

> Rollback overwrites the live file with the selected historical content. Use it carefully.

---



## 12. Trade-offs and Future Extensions

### Current strengths
- extremely simple implementation
- no third-party dependencies
- efficient deduplication
- easy to inspect and debug

### Current constraints
- store grows without cleanup unless GC is added
- version retention policy is not automated
- configuration is file-based rather than database-driven

### Natural next steps
- directory recursion instead of single-file tracking
- garbage collection for removed/old chunks
- retention policies (N latest, date-based retention, etc.)
- dedicated checkout or restore-to-directory mode for filesystem-like previous-version views

---

## Summary

shadowd uses a classic content-addressed backup pattern:

- split files into content-defined chunks
- store unique chunks once by hash
- keep a manifest of which chunk list represents each version
- rebuild old versions by concatenating stored chunks

This is a compact, low-overhead design that preserves historical versions while minimizing storage waste.
