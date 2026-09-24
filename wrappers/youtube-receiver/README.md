# YouTube receiver bridge

Private Node worker for `yt-cast-receiver` **2.1.0**, pinned by npm lockfile and
upstream commit `bec77aceb537aa63a7bd67cb2fb3b4ad1139e9e8`. The unpublished
2.1.1 source revision is not used. The package declares MIT, but neither the pinned source nor the published
package includes a standalone license text. Preserve package metadata; obtaining
the missing copyright/license notice remains a distribution gate. Upstream also
flags its forked peer-dial dependency as non-commercial; see the third-party
inventory before any distribution.

The upstream library owns YouTube Lounge/TV Code and DIAL. Our `Player` adapter
sends finite commands to one foreground Android client through the gateway;
the existing embedded player resolves and plays media through gateway tickets.
Provider account tokens and Lounge keys never go to the TV. TV Codes are
transient pairing codes shown only to the owning client, never logged or saved.

## Full

```sh
make youtube-up                 # Search/stream-resolution worker
make youtube-receiver-check     # Locked dependencies and mailbox tests
make youtube-receiver-up        # Private worker + managed provider configuration
make youtube-receiver-smoke     # Isolated live TV Code/DIAL description test
```

Setup preserves `.local/youtube-receiver/receiver.json` and the matching managed
`youtube_receiver` entry in `.local/gateway/config/providers.json`. The worker
starts idle. Open **Settings → YouTube receiver → Enable** on the paired client
(or the YouTube section's receiver action), then enter its code on your phone.
Closing the dialog keeps the foreground lease active; Disable or leaving the
app releases it. A crashed/unreachable client expires after 45 seconds.

Full uses host networking for DIAL/SSDP. Private HTTP **8095** requires the random
worker bearer token on every route, including health. Public DIAL HTTP **8096**
and SSDP UDP **1900** exist only while the receiver is active. `dialAddresses`
can restrict advertisement/binding to chosen host interfaces; its default uses
upstream interface discovery. LAN isolation/firewall/interface configuration must
be validated on the intended network. The gateway reaches the private API using
`host.docker.internal`; no token belongs in client configuration.

One pending command, a 35-second command timeout, 20-second initialization watchdog,
8 private HTTP connections, 256 MiB Node heap and 512 MiB container limit bound
work. The previous 192 MiB heap exhausted repeatedly during the first physical
evaluation; this larger bounded allocation requires a sustained receiver retest
and does not rule out an upstream memory leak. The gateway and client also have
finite HTTP deadlines. A play acknowledgement
requires the new player to buffer and enter PLAYING. Repeated acknowledgements
are idempotent, stale/preempted commands cannot block subsequent polling, and a
local volume error cannot be reported as a successful change. The worker may
restart if upstream initialization/shutdown hangs. Lounge state is memory-only.

## Edge

Run `bash gateway-edge/install-youtube-receiver.sh` **inside Termux**, after the
shared gateway and native Node/npm/Python are installed. Enable both `youtube`
and `youtube_receiver` in `~/.zombie/config/providers.json`, and explicitly enable
their runit services. This installer leaves the receiver stopped; private HTTP
binds to loopback. No Linux Node binary or Docker image is installed on Android.
Native execution and multicast compatibility have not been verified.

## Evidence and limits

The real pinned package obtained an anonymous TV Code, served the DIAL device
XML and stopped cleanly in an isolated Full container. Automated tests also cover
command ownership, protected playback resolution, acknowledgement timeouts and
client lifecycle races. They do **not** establish physical phone→TV playback,
LAN multicast discovery, audible output, seeking or queue completion. Automatic
next-video on natural completion and multiple receiver screens remain open.

Playback still depends on the YouTube resolution worker: progressive formats
may be unavailable. TV Code pairing does not authenticate a Google account or
remove upstream stream restrictions. Native/OEM DIAL selection and Advanced
backend overrides remain pending; this worker is the explicit compatibility
backend, not proof that a native implementation was tested and rejected.
This is not a certified Chromecast receiver.
