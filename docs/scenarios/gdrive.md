---
title: Backing up to Google Drive
layout: default
parent: Scenarios
nav_order: 9
description: "Land backups in Google Drive — a personal @gmail Drive over OAuth, or a Workspace Shared Drive with a service account."
---

# Backing up to Google Drive
{: .no_toc }

Use a Google Drive folder as a landing or offsite medium — a personal Google One
Drive over an OAuth token, or a Workspace Shared Drive with a service account.
{: .fs-6 .fw-300 }

1. TOC
{:toc}

---

## When to use this

You want cheap, already-paid-for cloud space (Google One, or a Workspace plan) as
a backup target, without standing up an S3 bucket. Google Drive is
**address-identified** like disk and cloud: the on-Drive layout is disk's verbatim
(`runs/<run>/` folders of clean payloads + `.hdr` sidecars), so a run streams
disk↔cloud↔gdrive unchanged and a plain download restores with stock tools.

NBackup only ever touches files it created — it uses the **`drive.file`** OAuth
scope, which is *non-sensitive*, so it needs neither broad Drive access nor
Google's restricted-scope security audit.

## Which authentication do I need?

Google Drive auth depends on your account type. The short version:

| Mechanism | Personal `@gmail` / Google One | Workspace (work/edu) |
|---|---|---|
| **OAuth user token** (`nb login`) | ✅ The only path that stores data | ✅ Works (uses the user's own Drive) |
| **Service account → its own Drive** | ❌ 0 GB usable quota | ❌ 0 GB usable quota |
| **Service account → Shared Drive** | ❌ Personal accounts have no Shared Drives | ✅ Cleanest unattended |

A bare service account cannot store data in "My Drive" (Google gives it 0 GB), so:

- **Personal Google Drive** → use the **OAuth token** path below.
- **Workspace** → create a **Shared Drive**, add a service account to it, and use
  the **service-account** path.

Either way, credentials come from `GOOGLE_APPLICATION_CREDENTIALS` and **never**
the config file, so the config is safe to commit.

## Config

```yaml
media:
  gdrive:
    type: gdrive
    folder: 0A--YOUR-FOLDER-OR-SHARED-DRIVE-ID   # from the folder's URL
    # prefix: nbackup/    # optional: a subfolder under `folder`
    capacity: 2TB
    # throughput: 20MB/s  # optional: cap the uplink (see Media)
landing: gdrive
```

`folder` is a Drive **folder ID** — the last segment of the folder's URL
(`https://drive.google.com/drive/folders/`**`0A…`**) — or a **Shared Drive ID**.

## Path A — personal Google Drive (OAuth token)

OAuth needs a registered client. NBackup ships none (no shared app, no shared
quota, no verification burden), so you create your own **once**:

1. In the [Google Cloud Console](https://console.cloud.google.com), create a
   project and **enable the Google Drive API**.
2. Configure the **OAuth consent screen** (User type: *External*). Because
   `drive.file` is non-sensitive, you can **Publish** it to *Production* with no
   verification review — this stops the refresh token from expiring after 7 days.
3. Create an **OAuth client ID** of type **Desktop app** and **download** its
   `client_secret.json`.
4. Bootstrap the token — headless, so it works over SSH on a server:

   ```bash
   export GOOGLE_APPLICATION_CREDENTIALS=~/.config/nbackup/gdrive-token.json
   nb login gdrive --client ~/Downloads/client_secret.json
   ```

   `nb login` prints a URL. Open it on **any** device, sign in, and grant access.
   Your browser then tries to open a `http://localhost/?code=…` page that **won't
   load** — that's expected; copy the `code` value from its address bar (or paste
   the whole URL) back into the terminal. NBackup writes the token to
   `$GOOGLE_APPLICATION_CREDENTIALS`.

5. `nb check` then confirms the medium opens; `nb dump` lands the first run.

## Path B — Workspace Shared Drive (service account)

Fully unattended, no `nb login`:

1. In the Google Cloud Console, create a project, **enable the Drive API**, create
   a **service account**, and download its **JSON key**.
2. In Google Drive, create a **Shared Drive**, then **share it with the service
   account's email** (as *Content manager*).
3. Use the Shared Drive's ID as `folder`, and point the environment at the key:

   ```bash
   export GOOGLE_APPLICATION_CREDENTIALS=/etc/nbackup/gdrive-sa.json
   nb check
   nb dump
   ```

No login step: the service-account key authenticates directly.

## Notes

- **Large archives** are split into `≤ part_size` ordered part-files (default
  10 GiB) for upload resumability; `cat …-L0.tar.gz.p* | tar xz` reconstructs one
  by hand.
- **Selective restore** (`nb recover`) uses Drive's ranged download, paying for the
  covering frames' bytes rather than the whole archive.
- **Offsite tiering** works as with any medium: dump to disk, then
  `nb sync --to gdrive` to mirror sealed runs to Drive.
- Google Drive bills **no egress or per-request** charge, so `nb plan`'s cost line
  prices storage only (a rough Google One estimate; override via `cost:`).
