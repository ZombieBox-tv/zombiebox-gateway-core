# AirPlay worker

Pinned UxPlay decrypts negotiated AirPlay input and forwards H.264 video and L16
audio over loopback RTP. Bounded FFmpeg bridges produce a 360p H.264 baseline/AAC
HLS stream and a separate AAC-only HLS stream. The separate audio path does not
wait for video, so audio-only senders have a receiver path. Gateway tickets proxy
the private playlists/segments; Android selects AirPlay video or AirPlay audio
through its existing embedded player.

Run `make services-build`, then `make airplay-up`. UxPlay uses host networking for
LAN discovery, TCP/UDP ports 35000–35002, and a generated four-digit PIN in private
`.local/airplay/worker.json`. RTP bridge ports 35010–35015 are loopback-only. The
private HTTP worker uses port 8093 with a bearer token. The gateway reaches it via
`host.docker.internal`; firewall reachability is an operator/device test step.
No host firewall changes are applied automatically.

`make services-smoke` verifies synthetic RTP to HLS, including audio without video.
That evidence does not validate iOS negotiation, metadata/artwork, PIN UX,
sender disconnect/reconnect, A/V synchronization or physical legacy playback.
Select the matching source after starting AirPlay; automatic receiver routing
and Now Playing interruption/restore remain open. HLS state is disposable and
bounded to recent segments. Capture/protected-content restrictions remain those
of the sender and upstream protocol implementation.

UxPlay: https://github.com/FDH2/UxPlay,
commit `57ea83411d5f7e0b38c5841987439340543f025c` (GPL-3.0).
No upstream source modifications. Image includes license/commit; source is
restored by `make references`. Full is experimental; native UxPlay on Termux is
not implemented or advertised as supported.
