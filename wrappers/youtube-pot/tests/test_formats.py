import sys
import unittest
import urllib.parse
from pathlib import Path

WRAPPER_DIR = Path(__file__).resolve().parents[1]
if str(WRAPPER_DIR) not in sys.path:
    sys.path.insert(0, str(WRAPPER_DIR))

from formats import (
    FormatSelector,
    format_tier,
    is_aac_audio,
    is_h264_video,
    is_safe_fps,
)


class FormatsTests(unittest.TestCase):
    def test_codec_filtering(self):
        self.assertTrue(is_h264_video({"vcodec": "avc1.4d401f"}))
        self.assertTrue(is_h264_video({"vcodec": "avc1.640028"}))
        self.assertTrue(is_h264_video({"vcodec": "h264"}))
        self.assertFalse(is_h264_video({"vcodec": "vp9"}))
        self.assertFalse(is_h264_video({"vcodec": "av01.0.05M.08"}))
        self.assertFalse(is_h264_video({"vcodec": "none"}))
        self.assertFalse(is_h264_video({}))

        self.assertTrue(is_aac_audio({"acodec": "mp4a.40.2"}))
        self.assertTrue(is_aac_audio({"acodec": "aac"}))
        self.assertFalse(is_aac_audio({"acodec": "opus"}))
        self.assertFalse(is_aac_audio({"acodec": "vorbis"}))
        self.assertFalse(is_aac_audio({"acodec": "none"}))
        self.assertFalse(is_aac_audio({}))

    def test_safe_fps_filtering(self):
        self.assertTrue(is_safe_fps({"fps": 30, "quality_label": "1080p"}))
        self.assertTrue(is_safe_fps({"fps": 24, "quality_label": "720p"}))
        self.assertTrue(is_safe_fps({"fps": 29.97, "quality_label": "480p"}))
        self.assertFalse(is_safe_fps({"fps": 60, "quality_label": "1080p60"}))
        self.assertFalse(is_safe_fps({"fps": 50, "quality_label": "720p50"}))
        self.assertFalse(is_safe_fps({"quality_label": "1080p60fps"}))
        self.assertFalse(is_safe_fps({"format_note": "60fps"}))

    def test_format_tier_boundary_mappings(self):
        # Numeric height boundary checks
        self.assertEqual(format_tier({"height": 2160}), "2160p")
        self.assertEqual(format_tier({"height": 1440}), "1440p")
        self.assertEqual(format_tier({"height": 1080}), "1080p")
        self.assertEqual(format_tier({"height": 720}), "720p")
        self.assertEqual(format_tier({"height": 480}), "480p")
        self.assertEqual(format_tier({"height": 360}), "360p")
        self.assertEqual(format_tier({"height": 240}), "360p")

        # Label fallback checks
        self.assertEqual(format_tier({"quality_label": "4K"}), "2160p")
        self.assertEqual(format_tier({"quality_label": "2160p"}), "2160p")
        self.assertEqual(format_tier({"quality_label": "1440p"}), "1440p")
        self.assertEqual(format_tier({"quality_label": "1080p"}), "1080p")
        self.assertEqual(format_tier({"quality_label": "720p"}), "720p")
        self.assertEqual(format_tier({"quality_label": "480p"}), "480p")
        self.assertEqual(format_tier({"quality_label": "360p"}), "360p")

    def test_higher_tiers_not_relabelled_as_1080p(self):
        # Ensure 2K and 4K formats are not relabeled or advertised as 1080p
        fmt_4k = {
            "format_id": "313",
            "url": "https://rr1.googlevideo.com/videoplayback?itag=313",
            "vcodec": "avc1.640033",
            "acodec": "none",
            "fps": 30,
            "height": 2160,
            "filesize": 200000,
        }
        fmt_2k = {
            "format_id": "271",
            "url": "https://rr1.googlevideo.com/videoplayback?itag=271",
            "vcodec": "avc1.640032",
            "acodec": "none",
            "fps": 30,
            "height": 1440,
            "filesize": 150000,
        }
        audio_fmt = {
            "format_id": "140",
            "url": "https://rr1.googlevideo.com/videoplayback?itag=140",
            "vcodec": "none",
            "acodec": "mp4a.40.2",
            "abr": 128,
            "filesize": 15000,
        }

        def mock_fetch(url, headers):
            return 206, {"content-range": "bytes 0-1023/50000"}, b"x" * 1024

        selector = FormatSelector([fmt_4k, fmt_2k, audio_fmt], fetch_func=mock_fetch)
        resolved, variants, _ = selector.determine_variants_and_resolve("1080p")

        # 4K and 2K must NOT appear in variants or be served as 1080p
        self.assertNotIn("2160p", variants)
        self.assertNotIn("1440p", variants)
        self.assertNotIn("1080p", variants)
        self.assertIsNone(resolved)

    def test_validation_cache_keyed_by_url_and_size_not_format_id(self):
        # Two formats sharing identical format_id (136) but having different URLs
        # One URL returns 206 (valid), the other returns 403 (forbidden).
        fmt_good = {
            "format_id": "136",
            "url": "https://rr1.googlevideo.com/videoplayback?itag=136&token=good",
            "filesize": 50000,
        }
        fmt_bad = {
            "format_id": "136",
            "url": "https://rr1.googlevideo.com/videoplayback?itag=136&token=bad",
            "filesize": 50000,
        }

        def mock_fetch(url, headers):
            if "token=good" in url:
                range_hdr = headers.get("Range", "")
                parts = range_hdr.replace("bytes=", "").split("-")
                start, end = int(parts[0]), int(parts[1])
                return (
                    206,
                    {"content-range": f"bytes {start}-{end}/50000"},
                    b"x" * (end - start + 1),
                )
            if "token=bad" in url:
                return 403, {}, b"Forbidden"
            return 404, {}, b""

        selector = FormatSelector([fmt_good, fmt_bad], fetch_func=mock_fetch)

        # Validate the good format first
        self.assertTrue(selector.validate_format(fmt_good))
        self.assertFalse(selector.saw_403)

        # Validate the bad format with the SAME format_id
        # Must not reuse the cached True from fmt_good!
        self.assertFalse(selector.validate_format(fmt_bad))
        self.assertTrue(selector.saw_403)

        # Re-validating fmt_good still returns cached True
        self.assertTrue(selector.validate_format(fmt_good))

    def test_truthful_variants_and_auto_selection(self):
        mock_formats = [
            # Combined 360p H.264+AAC <=30fps
            {
                "format_id": "18",
                "url": "https://rr1.googlevideo.com/videoplayback?itag=18&clen=20000",
                "vcodec": "avc1.42001E",
                "acodec": "mp4a.40.2",
                "fps": 30,
                "height": 360,
                "filesize": 20000,
            },
            # Adaptive 720p H.264 <=30fps
            {
                "format_id": "136",
                "url": "https://rr1.googlevideo.com/videoplayback?itag=136&clen=50000",
                "vcodec": "avc1.4d401f",
                "acodec": "none",
                "fps": 30,
                "height": 720,
                "filesize": 50000,
            },
            # Adaptive 1080p H.264 60fps (SHOULD BE REJECTED)
            {
                "format_id": "299",
                "url": "https://rr1.googlevideo.com/videoplayback?itag=299&clen=100000",
                "vcodec": "avc1.64002a",
                "acodec": "none",
                "fps": 60,
                "height": 1080,
                "filesize": 100000,
            },
            # Adaptive 1080p VP9 <=30fps (SHOULD BE REJECTED - non-H.264)
            {
                "format_id": "248",
                "url": "https://rr1.googlevideo.com/videoplayback?itag=248&clen=90000",
                "vcodec": "vp9",
                "acodec": "none",
                "fps": 30,
                "height": 1080,
                "filesize": 90000,
            },
            # Adaptive AAC Audio
            {
                "format_id": "140",
                "url": "https://rr1.googlevideo.com/videoplayback?itag=140&clen=15000",
                "vcodec": "none",
                "acodec": "mp4a.40.2",
                "abr": 128,
                "filesize": 15000,
            },
            # Adaptive Opus Audio (SHOULD BE REJECTED - non-AAC)
            {
                "format_id": "251",
                "url": "https://rr1.googlevideo.com/videoplayback?itag=251&clen=14000",
                "vcodec": "none",
                "acodec": "opus",
                "abr": 160,
                "filesize": 14000,
            },
        ]

        def mock_fetch(url, headers):
            range_hdr = headers.get("Range", "")
            parts = range_hdr.replace("bytes=", "").split("-")
            start, end = int(parts[0]), int(parts[1])
            parsed = urllib.parse.urlsplit(url)
            clen_list = urllib.parse.parse_qs(parsed.query).get("clen", ["20000"])
            total = int(clen_list[0])
            return (
                206,
                {"content-range": f"bytes {start}-{end}/{total}"},
                b"x" * (end - start + 1),
            )

        selector = FormatSelector(mock_formats, fetch_func=mock_fetch)
        resolved, variants, saw_403 = selector.determine_variants_and_resolve(
            target_quality="auto"
        )

        self.assertFalse(saw_403)
        self.assertIsNotNone(resolved)
        # Truthful variants: 720p (video 136 + audio 140) and 360p (combined 18).
        # 1080p rejected because 60fps and VP9!
        self.assertEqual(variants, ["720p", "360p"])
        # For auto: baseline 360p combined is preferred
        self.assertIn("itag=18", resolved["url"])
        self.assertNotIn("audioUrl", resolved)
        self.assertEqual(resolved["variants"], ["720p", "360p"])

    def test_explicit_quality_selection(self):
        mock_formats = [
            {
                "format_id": "18",
                "url": "https://rr1.googlevideo.com/videoplayback?itag=18&clen=20000",
                "vcodec": "avc1.42001E",
                "acodec": "mp4a.40.2",
                "fps": 30,
                "height": 360,
                "filesize": 20000,
            },
            {
                "format_id": "136",
                "url": "https://rr1.googlevideo.com/videoplayback?itag=136&clen=50000",
                "vcodec": "avc1.4d401f",
                "acodec": "none",
                "fps": 30,
                "height": 720,
                "filesize": 50000,
            },
            {
                "format_id": "140",
                "url": "https://rr1.googlevideo.com/videoplayback?itag=140&clen=15000",
                "vcodec": "none",
                "acodec": "mp4a.40.2",
                "abr": 128,
                "filesize": 15000,
            },
        ]

        def mock_fetch(url, headers):
            range_hdr = headers.get("Range", "")
            parts = range_hdr.replace("bytes=", "").split("-")
            start, end = int(parts[0]), int(parts[1])
            parsed = urllib.parse.urlsplit(url)
            clen_list = urllib.parse.parse_qs(parsed.query).get("clen", ["50000"])
            total = int(clen_list[0])
            return (
                206,
                {"content-range": f"bytes {start}-{end}/{total}"},
                b"x" * (end - start + 1),
            )

        selector = FormatSelector(mock_formats, fetch_func=mock_fetch)
        resolved, variants, saw_403 = selector.determine_variants_and_resolve(
            target_quality="720p"
        )

        self.assertFalse(saw_403)
        self.assertIsNotNone(resolved)
        self.assertIn("itag=136", resolved["url"])
        self.assertIn("itag=140", resolved.get("audioUrl", ""))
        self.assertEqual(resolved["variants"], ["720p", "360p"])

    def test_audio_unavailable_drops_adaptive_tier(self):
        # Only adaptive 720p video, NO AAC audio (only opus)
        mock_formats = [
            {
                "format_id": "136",
                "url": "https://rr1.googlevideo.com/videoplayback?itag=136&clen=50000",
                "vcodec": "avc1.4d401f",
                "acodec": "none",
                "fps": 30,
                "height": 720,
                "filesize": 50000,
            },
            {
                "format_id": "251",
                "url": "https://rr1.googlevideo.com/videoplayback?itag=251&clen=14000",
                "vcodec": "none",
                "acodec": "opus",
                "abr": 160,
                "filesize": 14000,
            },
        ]

        def mock_fetch(url, headers):
            return 206, {"content-range": "bytes 0-1023/50000"}, b"x" * 1024

        selector = FormatSelector(mock_formats, fetch_func=mock_fetch)
        resolved, variants, _ = selector.determine_variants_and_resolve(
            target_quality="720p"
        )
        # Since no AAC audio exists, 720p cannot be played or offered!
        self.assertEqual(variants, [])
        self.assertIsNone(resolved)


if __name__ == "__main__":
    unittest.main()
