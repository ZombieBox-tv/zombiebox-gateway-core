# Rebrowser worker

`make services-build` builds the optional image; `make rebrowser-up` enables it.
Only the worker remains idle. Chromium starts when a paired client opens a URL.
Open **Browser** from the client Home. Use **Next link**, **Open link**, text entry
and the page's arrow keys. Back closes the native screen and releases the session.

Gateway commands: `POST /v1/browser`, `GET /v1/browser/{id}/frame`,
`POST /v1/browser/{id}/input`, `DELETE /v1/browser/{id}`. No upstream tokens or CDP
ports are exposed to clients. Session/profile lifetime, limits and known gaps are
in [ADR 0021](../../docs/adr/0021-rebrowser-maintained-chromium.md).

`python3 scripts/smoke-browser.py` tests the real image with a public example page;
it does not claim that the Android screen has run on Dalvik or a physical remote.

The seccomp profile is adapted from Microsoft Playwright v1.58.2,
commit `ce480a952553175eae75342aad2c5e86cdf2cbba`,
`utils/docker/seccomp_profile.json` (Apache-2.0; see `LICENSE.playwright`).
The only policy change is an unconditional `chroot` syscall allowance: the
non-root container has no outer capabilities, while Chromium needs chroot inside
its new user namespace. Chromium's sandbox remains enabled. This dependency
supports packaging only; Playwright is not a product runtime dependency.

Puppeteer Core is Apache-2.0 and is pinned in `package-lock.json`. Its source/docs
clone and the packaging reference are restored by `make references`. Chromium
and its bundled dependencies retain their package-provided notices.
