# PRD: NBackup(Working Title)

## Vision

Create a modern backup system inspired by Amanda that preserves its strongest operational properties while embracing cloud and object storage.

The system should be understandable without specialized knowledge, produce portable backup artifacts, support both S3 and tape, and automatically adapt backup planning to a storage budget rather than a fixed tape rotation.

Core philosophy:

> A backup administrator should be able to reason about backups by looking at a sequence of immutable daily backup slots rather than a database of chunks.

---

# Goals

## Preserve Amanda's strengths

### Balanced backup scheduling

The planner should distribute full and incremental backups across days to avoid backup spikes.

Users should not need to manually schedule full backups.

### Immutable daily backup artifacts

Each backup run produces exactly one immutable slot.

Example:

```text
slot-2026-06-20/
slot-2026-06-21/
slot-2026-06-22/
```

Once sealed, a slot is never modified.

### Human-readable backup contents

Backups should be stored as normal tar archives.

A user must be able to restore data using standard tools.

Example:

```bash
tar -xf app-home-L0.tar
```

No proprietary chunk store is required for recovery.

### Tape-cycle safety

The system should preserve the psychological and operational safety of Amanda's tape cycle.

Users should never feel that yesterday's backup can immediately overwrite last month's backup.

### Tape support

Tape remains a first-class storage medium.

Tape should not be treated as legacy functionality.

---

# Non-Goals

## Global deduplication

The system is not optimized for maximum storage efficiency.

Operational simplicity is preferred over extreme deduplication.

## Chunk-store architecture

The system is not based on content-addressed chunk databases.

Examples of intentionally avoided designs:

* Veeam-style block stores
* Restic-style chunk repositories
* Borg-style deduplicated archives

---

# Core Concepts

## DLE

A backup source.

Examples:

```yaml
sources:
  - host: app01
    path: /home

  - host: db01
    path: /var/lib/postgresql
```

---

## Run

A single planner execution.

Typically daily.

Example:

```text
2026-06-20
```

---

## Slot

The primary backup artifact.

A run produces exactly one slot.

Example:

```text
slot-2026-06-20/
```

A slot is immutable after sealing.

A slot represents the complete output of a run.

---

## Cycle

A safety boundary controlling deletion and tape reuse.

Examples:

```yaml
cycle:
  minimum_age: 30d
  require_verified_successor: true
```

A slot cannot be deleted until:

* it is outside the cycle
* a newer valid recovery path exists

---

## Media

Storage locations for slot copies.

Examples:

```yaml
media:
  - s3-hot
  - glacier
  - tape
  - local-disk
```

---

# Slot Format

A slot is represented as a normal directory.

Example:

```text
slot-2026-06-20/
  SLOT.json
  MANIFEST.json
  CHECKSUMS.sha256

  archives/
    app-home-L0.tar.zst
    db-var-L1.tar.zst
```

Properties:

* self-contained
* immutable
* copyable
* inspectable
* understandable without backup software

---

# Landing Medium

Every slot is first written to a landing medium.

Examples:

```yaml
landing:
  media: s3
```

or

```yaml
landing:
  media: local-disk
```

The landing medium becomes the authoritative location for slot creation.

A local holding disk is not required.

---

# S3 Operation

Direct-to-S3 is a first-class deployment model.

Example:

```text
client
   ↓
slot creation
   ↓
S3
```

Workflow:

1. Upload archive files
2. Upload manifests
3. Verify checksums
4. Write SLOT.json
5. Mark slot sealed

After sealing, the slot becomes immutable.

---

# Tape Operation

Tape stores copies of sealed slots.

Workflow:

```text
sealed slot
     ↓
tar stream
     ↓
tape
```

The slot format does not change.

Tape acts as a transport and storage medium.

---

# Tape Format

Each slot is serialized as a tar stream.

Example:

```bash
tar -cf - slot-2026-06-20/
```

The tar stream is written to tape.

A slot may span multiple tapes.

Example:

```text
slot-2026-06-20

TAPE-0042
  part 1

TAPE-0043
  part 2
```

This is a media concern, not a slot concern.

The slot remains a single logical artifact.

---

# Planning Model

Users specify storage budgets rather than tape counts.

Example:

```yaml
storage:
  budget: 20TB
```

The planner automatically chooses:

* backup levels
* full backup frequency
* retention depth

while remaining inside the budget.

---

# Planner Objectives

Priority order:

## 1. Preserve recoverability

Never delete the last valid recovery path.

## 2. Respect cycle safety

Never retire slots too aggressively.

## 3. Stay within storage budget

Adapt scheduling and retention automatically.

## 4. Balance daily backup volume

Avoid large backup spikes.

---

# Example Configuration

```yaml
storage:
  budget: 20TB

cycle:
  minimum_age: 30d
  require_verified_successor: true

landing:
  media: s3

media:
  s3:
    bucket: company-backups

  tape:
    enabled: true
    retention: 180d

sources:
  - host: app01
    path: /home

  - host: db01
    path: /var/lib/postgresql
```

---

# Design Principles

## Slots are the primary abstraction

Not tapes.

Not chunks.

Not databases.

A user should think in terms of:

```text
slot-2026-06-20
slot-2026-06-21
slot-2026-06-22
```

## Media are secondary

The same slot may exist on:

* S3
* Glacier
* Tape
* Disk

without changing its format.

## Simplicity over optimization

Prefer:

```text
normal tar archives
immutable slots
simple retention
```

over:

```text
global chunk stores
cross-backup deduplication
opaque repositories
```

## Operational understandability

A backup administrator should be able to answer:

> What happened on June 20?

by inspecting:

```text
slot-2026-06-20/
```

without requiring a running backup server or metadata database.
