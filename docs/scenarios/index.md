---
title: Scenarios
layout: default
nav_order: 6
has_children: true
description: "Complete, copy-ready NBackup setups: single machine, disk-to-S3, cloud-only, tape with a holding disk, robotic libraries, remote hosts, and a full 3-2-1-1-0 deployment."
---

# Scenarios

Complete, copy-ready setups for common situations. Each scenario gives the full
config, the commands to run it, and notes on what to watch. Pick the one closest
to yours and adapt.

| Scenario | Shape |
|---|---|
| [Single machine → local disk](single-machine) | One host, backups on a local disk. The simplest useful setup. |
| [Disk → S3 offsite](disk-to-s3) | Land fast on local disk, replicate to S3. No holding disk. |
| [Cloud-only](cloud-only) | Dump straight to an object store (S3/compatible). No local copy. |
| [Tape with a holding disk](tape-holding-disk) | A fast disk buffers parallel dumps and feeds one tape drive at disk speed. |
| [S3 with a holding disk](s3-holding-disk) | A local buffer absorbs dumps, then drains to a bandwidth-capped cloud tier. |
| [Robotic tape library](tape-library) | A multi-bay changer with automatic label rotation. |
| [Remote hosts over SSH](remote-hosts) | Back up several remote machines with no agent installed. |
| [Full 3-2-1-1-0 deployment](full-321) | Disk + offsite + immutability + drills + alerting, end to end. |

Each scenario links to the [Features](../features) pages for the mechanics behind
it. If you're new, start with [Single machine](single-machine).
