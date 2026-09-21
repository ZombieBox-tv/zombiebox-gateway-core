# Threadfin package

Run `make services-build`, then `make threadfin-up`. Open
http://127.0.0.1:34400/web/ to import/filter/map your M3U and XMLTV sources.
Private persistent state lives in `.local/threadfin`. The administrative UI is
bound to host loopback; playback exports are accessible on the Compose network.

After configuring channels, select these gateway IPTV endpoints:

```json
{"iptv":{"enabled":true,"url":"http://threadfin:34400/m3u/threadfin.m3u","epgUrl":"http://threadfin:34400/xmltv/threadfin.xml"}}
```

`python3 scripts/setup-services.py --enable threadfin` sets these endpoints only
when it will not overwrite an existing IPTV source. Direct M3U/XMLTV remains the
lighter default and is sufficient for the author's public list. Enabling this
package does not install a second IPTV provider or duplicate gateway logic.

The source snapshot's vendor manifest is inconsistent with its go.mod. Build
with `-mod=readonly` against the pinned module checksums, preserving the upstream
clone. No `go mod tidy`, dependency upgrade or vendor rewrite is applied.

Native Edge candidate: `gateway-edge/install-services.sh threadfin`. Native
compilation/execution, memory behavior and Android network discovery need device
evidence. Full smoke checks real packaged HTTP discovery and writable state;
real channel/EPG mapping and restream throughput remain untested.

Upstream: https://github.com/Threadfin/Threadfin,
commit `6b9c0ccf16164eb362af0a44660228267734c5aa` (MIT).
Image includes upstream license and commit; `make references` restores source.
