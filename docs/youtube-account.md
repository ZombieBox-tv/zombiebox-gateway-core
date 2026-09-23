# YouTube account browsing

The YouTube account path is independent of TV Code/DIAL and of the private
YouTube.js worker token. The Gateway uses Google's limited-input OAuth device
flow and requests only `youtube.readonly`. The operator supplies a Google Cloud
OAuth client of type **TVs and Limited Input devices**, with the YouTube Data API
enabled. Configure `ZOMBIE_YOUTUBE_OAUTH_CLIENT_ID` and optional
`ZOMBIE_YOUTUBE_OAUTH_CLIENT_SECRET` in the Gateway's private environment. Empty
values leave anonymous YouTube and receiver functions unaffected.

A paired Client starts authorization at `POST /v1/youtube/account/authorization`.
The response contains only the user code, verification URL, expiry and minimum
poll interval; the Google device code stays in Gateway memory. The Client polls
`POST /v1/youtube/account/authorization/poll` no sooner than that interval.
The first 428 `authorization_pending` response remains a semantic pending state;
`slow_down` extends the interval. Successful access/refresh tokens are written
to private SQLite. Expired access tokens refresh on demand. `DELETE
/v1/youtube/account` requires the current operator code and removes the local
grant even if remote revocation is unreachable. A restart during unfinished
authorization requires a new code; a connected account survives restart.

Subscriptions and playlists use Google's read-only Data API. Gateway endpoints
return bounded semantic pages and opaque page tokens; no Google DTO or token
crosses to the Client. Channel/playlist roots are retained under device-scoped
opaque `browseId` values, then traverse the existing pinned YouTube worker. The
worker must be enabled to open a returned root, but account listing does not
make an unavailable worker a startup dependency. At most 40 items/page and
20 UI pages are exposed. Account data is household-scoped on this Gateway; any
paired Client can browse it. Only the local operator code can disconnect it.

Host tests use a fake authorization/Data API and a fake YouTube worker. A real
Google Cloud client, consent screen, quota, account and Android screen have not
been validated. Browser/list behavior with a real account remains an acceptance
gate; OAuth provider policy and tokens may change independently of this code.
