# Contributing to NBackup

Thanks for your interest in improving NBackup. This document covers the legal
side of contributing; for the technical map of the codebase, start with
[ARCHITECTURE.md](ARCHITECTURE.md).

## License of contributions

NBackup is licensed under the **GNU General Public License v3.0** (see
[LICENSE](LICENSE)). By contributing, you agree that your contribution is
licensed under the same terms — the project follows the *inbound = outbound*
convention: what you send in is licensed exactly as the project is licensed out.

We do **not** require a Contributor License Agreement (CLA). Instead we use the
**Developer Certificate of Origin (DCO)**: a lightweight, per-commit statement
that you wrote the patch or otherwise have the right to submit it under the
project's license.

## Signing off your commits (DCO)

Every commit must carry a `Signed-off-by` line matching the commit author. Add
it automatically with the `-s` flag:

```bash
git commit -s -m "your message"
```

This appends a trailer like:

```
Signed-off-by: Jane Developer <jane@example.com>
```

By signing off you certify the [Developer Certificate of Origin](https://developercertificate.org/)
(reproduced below).

<details>
<summary>Developer Certificate of Origin 1.1</summary>

```
By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```

</details>

## Submitting changes

Before opening a pull request, make sure the change is verified:

```bash
gofmt -l .        # must print nothing
go vet ./...
go test -race ./...
```

Keep changes focused, follow the conventions and layering rules described in
[ARCHITECTURE.md](ARCHITECTURE.md), and match the style of the surrounding code.
