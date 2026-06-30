# Security Policy

NBackup is early-stage software. We take security seriously — a backup tool sits
on the integrity and confidentiality of other people's data — and we appreciate
reports that help us keep it sound.

## Supported versions

NBackup has not yet reached a stable release. Security fixes are applied to the
`main` branch; there is no back-port stream for tagged releases yet. Always run
the latest `main` until a versioned support policy is published here.

## Reporting a vulnerability

**Please do not open a public issue for security vulnerabilities.**

Report privately through either channel:

- **GitHub** — use [Report a vulnerability][advisory] under the repository's
  **Security** tab to open a private security advisory.
- **Email** — write to **marcus.nilsson@niloen.com** with the details below.

Please include:

- a description of the issue and the impact you foresee,
- the steps or a proof of concept needed to reproduce it,
- the affected commit or `main` revision,
- and any suggested remediation if you have one.

## What to expect

- We aim to acknowledge a report within **5 business days**.
- We will keep you updated as we investigate and work on a fix.
- Once a fix is ready we will coordinate disclosure with you and credit you in
  the advisory unless you prefer to remain anonymous.

Thank you for helping keep NBackup and its users safe.

[advisory]: https://github.com/Niloen/nbackup/security/advisories/new
