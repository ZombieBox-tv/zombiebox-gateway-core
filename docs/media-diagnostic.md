# Local software pipeline diagnostic

Run `zombied --diagnose-media` explicitly. The command exits after printing a
credential-free JSON report; it never opens gateway state/configuration or starts
the HTTP server. It creates a private synthetic one-second H.264/AAC fixture,
passes it through the actual remux and transcode adapters, then probes and decodes
each output. Both audio and video must produce decoded frames. Tool presence or
successful metadata probing alone cannot produce a pass.

The whole run has a 30-second deadline, bounded process output, existing media job
limits and temporary private files that are removed on return. It uses software
encoding; no hardware acceleration is inferred. Missing tools/busy capacity remain
unavailable and cancellation is distinct from pipeline failure. No raw process
stderr, filenames, provider URLs or credentials enter the report.

`gatewayPipelineVerified` describes only the local synthetic software path.
`receiverValidated` and `networkValidated` remain false. The diagnostic does not
validate device decoders, RTSP/RTP, account access, arbitrary media formats or
long-duration load. It does not update a client's capability/probe records.

Full can run the same command in its gateway image with a private bounded `/tmp`.
New Edge sources expose it through `zombiebox doctor --media`; older core bundles
report unavailable/unsupported rather than successful media validation.
