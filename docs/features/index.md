---
title: Features
layout: default
nav_order: 5
has_children: true
description: "Each major NBackup capability — planning, archivers (tar, PostgreSQL, pipe), media, replication, encryption, drills, recovery, pruning, holding disk, remote sources, and monitoring."
---

# Features

NBackup's capabilities, one page each. Each page explains what the feature does,
why it works the way it does, and how to configure and use it.

| Feature | What it covers |
|---|---|
| [Planning & scheduling](planning) | Multilevel levels, the bump rule, automatic promotion, the two capacity limits, forecasting. |
| [Cost forecasting](cost) | `$/month` storage and egress estimates for cloud media — fully offline. |
| [Archivers](archivers) | What produces the dump stream — GNU tar (default), live PostgreSQL clusters (17+ incremental base backups), and bring-your-own-command pipes. |
| [Storage media](media) | Disk, tape (libraries & single drives), and cloud object stores. |
| [Replication & tiered storage](replication) | `nb copy` and `nb sync` — land fast, replicate offsite. |
| [Encryption](encryption) | Source-tied `gpg` encryption that keeps copies interchangeable. |
| [Verification & drills](verification) | `nb verify` (integrity) and `nb drill` (proven recoverability — the **0** of 3-2-1-1-0). |
| [Recovery](recovery) | Whole-DLE restore and interactive file-level recovery. |
| [Pruning & retention](pruning) | Per-medium retention, the safety floor, and capacity reclamation. |
| [Holding disk](holding-disk) | A fast scratch buffer that feeds a slow tape or cloud at disk speed. |
| [Remote sources over SSH](remote-sources) | Back up remote hosts with no NBackup software on the client. |
| [Monitoring & reporting](monitoring) | `nb status`, `nb report`, and pluggable failure alerting. |
| [Status website](web) | `nb web` — a read-only, mobile-friendly dashboard (overview, runs, media history, drills) that takes no lock. |

New here? Read [Concepts](../concepts) first for the vocabulary, then come back.
Looking for a complete worked setup? See [Scenarios](../scenarios).
