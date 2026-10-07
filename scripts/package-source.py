#!/usr/bin/env python3
"""현재 작업 사본의 소스만 묶는다. 비밀·런타임 데이터·빌드 산출물은 포함하지 않는다."""
import argparse
import subprocess
import tarfile
from pathlib import Path

parser = argparse.ArgumentParser(description="릴리스에 대응하는 현재 소스 묶음 생성")
parser.add_argument("output", type=Path)
args = parser.parse_args()
root = Path(__file__).resolve().parents[1]
tracked = subprocess.check_output(["git", "ls-files", "-z"], cwd=root).decode().split("\0")
# 새 한글화 파일도 커밋 전 검증 패키지의 대응 소스에 넣습니다.
new = ["NOTICE.md", "build-docker.sh", "docker-compose.ko.yml", "db/intercept_source_test.go", "report/report_test.go"]
for folder in ["scripts", "docs", "web/scripts", "web/public/legal", "web/src/app/about", "screenshots/ko"]:
    new.extend(str(p.relative_to(root)) for p in (root / folder).rglob("*") if p.is_file())
new.extend(["web/src/app/error.tsx", "web/src/app/global-error.tsx", "web/src/components/legal-notice.tsx", "web/src/config/legal-notice.ts", "web/src/lib/tool-display.ts", ".github/workflows/korean-check.yml"])
files = sorted(set(tracked + new))
args.output.parent.mkdir(parents=True, exist_ok=True)
with tarfile.open(args.output, "w:gz") as archive:
    for name in files:
        p = root / name
        if not name or not p.is_file() or p.is_symlink():
            continue
        if any(part in {".git", "node_modules", "__pycache__", ".next", "data", "dist"} for part in p.relative_to(root).parts):
            continue
        if name.startswith("docs/") and name not in {"docs/초보자-길잡이.md", "docs/포크-라이선스-안내.md", "docs/한글화-검증.md"}:
            continue
        if p.name in {".env", "jwt.key", "config.json"} or p.suffix in {".db", ".pyc"}:
            continue
        archive.add(p, arcname="ARTEX-source/" + name, recursive=False)
print("대응 소스 생성:", args.output)
