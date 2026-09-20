# Third-party inventory

Local sources/ clones preserve their upstream licenses and are excluded from the commit and APK. upstreams.lock.json records exact provenance. licenseHint is GitHub metadata, not a legal conclusion or replacement for the actual license file.

- Gradle Wrapper 8.13: https://github.com/gradle/gradle, Apache-2.0. Upstream scripts and JAR; JAR SHA-256: 81a82aaea5abcc8ff68b3dfcb58b3c3c429378efd98e7433460610fecd7ae45f. See gradle-LICENSE.txt (including upstream third-party attribution).
- Kotlin stdlib 2.3.0: JetBrains, Apache-2.0; bootstrap APK dependency. See kotlin-LICENSE.txt. Preserve bundled notices and inventory distribution requirements before release.
- Android SDK/build tools: Google licenses accepted outside the repository; not redistributed.
- FFmpeg: license combination depends on build/components. Read COPYING*, LICENSE.md and build configuration before packaging.
- go-librespot and UxPlay: upstream GPL licenses. External process boundaries do not remove distribution obligations.
- Serenity: MIT reference. Future code copies must retain copyright and license.
- yt-cast-receiver: inspect LICENSE before copying/distributing; do not infer license absence from GitHub metadata.
- resources/ui-concepto.png: user-provided reference including third-party brands/artwork; not included in the APK.

The author has not selected a license for first-party project code. Decide before publication.

- modernc.org/sqlite v1.38.2: BSD-3-Clause; Linux gateway dependency. See modernc-sqlite-LICENSE.txt and the locked source reference. SQLite itself is public-domain upstream code.
- github.com/mattn/go-sqlite3 v1.14.32: MIT; Android/Termux gateway driver (CGO), not an APK dependency. See go-sqlite3-LICENSE.txt.
- Go transitive dependency versions are locked by gateway/go.mod and go.sum. The distribution inventory must include their notices before a public binary release.
