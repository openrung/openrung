# Contributing to OpenRung

Thanks for your interest in contributing.

## License of contributions

OpenRung is licensed under the **GNU General Public License v3.0 or later**
(GPL-3.0-or-later); see [`LICENSE`](LICENSE). This is required because the
mobile clients statically link [sing-box](https://github.com/SagerNet/sing-box),
which is GPL-3.0-or-later.

By submitting a contribution, you agree that your contribution is licensed under
GPL-3.0-or-later, consistent with the project license (inbound = outbound).

## Developer Certificate of Origin (DCO)

We use the [Developer Certificate of Origin](https://developercertificate.org/)
to certify that you wrote, or otherwise have the right to submit, the code you
contribute. Sign off every commit:

```sh
git commit -s -m "your message"
```

This adds a `Signed-off-by: Your Name <you@example.com>` trailer, which certifies
the DCO (reproduced below). Pull requests whose commits are not signed off will
be asked to amend.

> If the project later prefers a Contributor License Agreement (e.g. assigning
> stewardship to the OpenRung Foundation) to retain flexibility to grant
> distribution exceptions, this section will be updated.

<details>
<summary>Developer Certificate of Origin 1.1 (full text)</summary>

```
By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I have the right
    to submit it under the open source license indicated in the file; or

(b) The contribution is based upon previous work that, to the best of my
    knowledge, is covered under an appropriate open source license and I have
    the right under that license to submit that work with modifications,
    whether created in whole or in part by me, under the same open source
    license (unless I am permitted to submit under a different license), as
    indicated in the file; or

(c) The contribution was provided directly to me by some other person who
    certified (a), (b) or (c) and I have not modified it.

(d) I understand and agree that this project and the contribution are public and
    that a record of the contribution (including all personal information I
    submit with it, including my sign-off) is maintained indefinitely and may be
    redistributed consistent with this project or the open source license(s)
    involved.
```
</details>

## License headers

New first-party source files should carry an SPDX header so their license is
unambiguous:

- Go: `// SPDX-License-Identifier: GPL-3.0-or-later`
- Kotlin / Swift: `// SPDX-License-Identifier: GPL-3.0-or-later`

## Third-party dependencies

If you add, remove, or upgrade a dependency that is **shipped** to users (in an
app, a Docker image, or a server binary), update
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) accordingly. Do **not** vendor
or patch MPL/GPL upstreams (e.g. Xray-core) in-tree without flagging it — that
changes our source-disclosure obligations.
