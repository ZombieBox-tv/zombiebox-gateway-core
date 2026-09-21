#!/usr/bin/env python3
"""Small dependency guards for boundaries established by ADR 0024."""

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
errors = []
for project in ("app", "."):
    for path in (ROOT / project / "src/main/java").rglob("*.kt"):
        source = path.read_text()
        relative = str(path.relative_to(ROOT))
        imports = re.findall(r"^import (.+)$", source, re.MULTILINE)
        if "/domain/" in relative or "/presentation/viewmodel/" in relative:
            for dependency in imports:
                if (
                    dependency.startswith(("android.", "androidx.", "org.json."))
                    or ".data." in dependency
                    or dependency.endswith("GatewayApi")
                ):
                    errors.append(
                        f"{relative}: domain/ViewModel imports infrastructure: {dependency}"
                    )
        if "/presentation/" in relative or path.name.endswith("Activity.kt"):
            if "org.json." in source or re.search(r"\bapi\.request\(", source):
                errors.append(
                    f"{relative}: presentation performs wire-level data access"
                )
        if "/domain/" in relative and "class " in source and "ViewModel" in source:
            errors.append(f"{relative}: ViewModel belongs in presentation")

for feature in ("playback", "devices", "receivers/youtube"):
    for path in (ROOT / "gateway/internal" / feature).glob("*.go"):
        source = path.read_text()
        if '"zombiebox.local/gateway/internal/server"' in source:
            errors.append(
                f"{path.relative_to(ROOT)}: application feature imports HTTP server"
            )

if errors:
    raise SystemExit("\n".join(errors))
print(
    "PASS: Android domain/ViewModel and presentation data boundaries; Go feature dependency direction"
)
