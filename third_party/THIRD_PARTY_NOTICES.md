# Third-party inventory

Local sources/ clones preserve their upstream licenses and are excluded from the commit and APK. upstreams.lock.json records exact provenance. licenseHint is GitHub metadata, not a legal conclusion or replacement for the actual license file.

- Gradle Wrapper 8.13: https://github.com/gradle/gradle, Apache-2.0. Upstream scripts and JAR; JAR SHA-256: 81a82aaea5abcc8ff68b3dfcb58b3c3c429378efd98e7433460610fecd7ae45f. See gradle-LICENSE.txt (including upstream third-party attribution).
- Kotlin stdlib 2.3.0: JetBrains, Apache-2.0; bootstrap APK dependency. See kotlin-LICENSE.txt. Preserve bundled notices and inventory distribution requirements before release.
- Android SDK/build tools: Google licenses accepted outside the repository; not redistributed.
- FFmpeg: license combination depends on build/components. Read COPYING*, LICENSE.md and build configuration before packaging.
- go-librespot and UxPlay: upstream GPL licenses. External process boundaries do not remove distribution obligations.
- Serenity: MIT reference. Future code copies must retain copyright and license.
- yt-cast-receiver 2.1.0: optional Node runtime dependency, exact npm lock and source commit bec77aceb537aa63a7bd67cb2fb3b4ad1139e9e8. package.json declares MIT, but the pinned checkout and published package contain no standalone license text. Retain package metadata and obtain the missing copyright/license notice before distribution; do not fabricate an attribution. Transitive npm licenses remain in the image and require the release inventory.
- resources/ui-concepto.png: user-provided reference including third-party brands/artwork; not included in the APK.

The author has not selected a license for first-party project code. Decide before publication.

- modernc.org/sqlite v1.38.2: BSD-3-Clause; Linux gateway dependency. See modernc-sqlite-LICENSE.txt and the locked source reference. SQLite itself is public-domain upstream code.
- github.com/mattn/go-sqlite3 v1.14.32: MIT; Android/Termux gateway driver (CGO), not an APK dependency. See go-sqlite3-LICENSE.txt.
- Go transitive dependency versions are locked by gateway/go.mod and go.sum. The distribution inventory must include their notices before a public binary release.

- YouTube.js 18.0.0: MIT, optional Node wrapper dependency; see youtube-js-LICENSE.txt. npm dependencies and their licenses remain in the image's node_modules; package-lock.json pins exact transitive versions.
- QuickJS Emscripten 0.31.0: MIT, bounded interpreter used only in the Node wrapper; see quickjs-emscripten-LICENSE.txt. Its WebAssembly runtime is not an Android APK native dependency. Retain package-bundled QuickJS/Emscripten and transitive notices before distribution.
- Historical Serenity v1.9.4 and the current pinned revision informed navigation strategy. No Serenity code was copied; docs/design/serenity-navigation.md records the comparison.

- MediaMTX 1.21.1, MIT: locked commit 048255986f7e04b859b4c4efe651448ec785ecd4, upstream Full image pinned by digest. See mediamtx-LICENSE.txt. wrappers/mediamtx/android.patch changes only Android build constraints in a disposable Edge build copy; preserve its upstream attribution. Edge omits the unused standalone HLS JavaScript player and Raspberry Pi camera support.
- github.com/wlynxg/anet v0.0.5, MIT: transitive MediaMTX Android interface compatibility helper, now cloned as a locked reference. Its Go linker exception and runtime verification limits are documented in ADR 0019.
- Full now installs Alpine ffmpeg 8.0.1-r1 (including libx264). The image has GPL/LGPL-covered media dependencies; a full package/SBOM/source-and-notice inventory remains a public distribution gate. No such media libraries are linked into either Android APK.

- yt-cast-receiver's README also flags its forked `peer-dial` dependency as free
  for non-commercial use and directs commercial users to obtain author consent.
  Do not describe the complete receiver dependency tree as uniformly MIT or
  unrestricted open source. Resolve this dependency/license scope before public
  distribution; the development checkpoint is not a distribution approval.
