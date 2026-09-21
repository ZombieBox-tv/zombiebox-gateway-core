# Coordinated receiver replacement

Dev.22 adds optional `replaceExisting` to media selection, YouTube receiver opening
and Cast creation. Omission preserves first-armed exclusion. Only the paired target
may select its media/YouTube receiver; the flag never replaces another device's
media or YouTube lease.

Cast also requires the target's `allowReceiverHandoff` preference when another
transport owns the screen. It defaults to false and does not replace `allowCasting`.
The grant preserves the current receiver until relay readiness succeeds. Consent
and session capacity are checked again at readiness. Failed relay or preparation
leaves the old receiver intact; successful replacement revokes old ownership and
stream sessions before publishing the new Cast plan. One Cast globally remains.

YouTube clients send `PlaybackRequest.receiverId`; the source and final plan must
belong to that active lease. In-flight resolution or polling cannot revive a revoked
lease. A confirmed lost lease returns 404 to polling and `receiver_changed` to a
stale resolution. Private cleanup is best effort with a two-second timeout; local
revocation does not depend on a working optional worker.

Media selection arms the new receiver even if no sender is currently playing.
It is an explicit switch, not automatic simultaneous listening across protocols.
No physical/OEM/account acceptance is inferred from host regression checks.
