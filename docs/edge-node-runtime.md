# Node runtime selection for Edge

The locked YouTube catalog and TV receiver workers support Node 22.22.2–22.x and
Node 24.18.0–24.x. Node 22.22.1 is intentionally excluded: 22.22.2 was a security
release. The official Termux `nodejs-lts` recipe currently targets 24.18.0, while
the separate TUR `nodejs-22` recipe remains at 22.22.1. Prefer the official Termux
package for a compiler-free Edge installation; do not silently lower the security
floor or use a Linux/glibc Node binary on Android.

Host evidence: fresh `npm ci --ignore-scripts --omit=dev` installs for each worker
under Linux Node 24.18.0, all nine catalog and five receiver tests passing, and no
`.node`/`.so` files in the installed npm payloads. This establishes a portable JS
candidate, not Bionic Node execution, account login or TV Code reception. Full's
published dev.46 images remain pinned to Node 22.22.2 and are unchanged.

The Edge release must still package both workers, verify corresponding source and
third-party notices, install them stopped, and validate the native Node package on
ARMv7/ARM64 before marking that optional module delivered.
