#!/usr/bin/env python3
"""한글 파일명을 UTF-8로 저장하고 실행 파일 권한을 보존하는 ZIP 생성기."""
import argparse
from pathlib import Path
from zipfile import ZipFile, ZIP_DEFLATED

parser = argparse.ArgumentParser(description="한글 문서명을 보존하는 릴리스 ZIP 생성")
parser.add_argument("package_root", type=Path)
parser.add_argument("output", type=Path)
args = parser.parse_args()
root = args.package_root.resolve(strict=True)
with ZipFile(args.output, "w", compression=ZIP_DEFLATED, compresslevel=9) as archive:
    for path in sorted(root.rglob("*")):
        archive.write(path, arcname=path.relative_to(root.parent).as_posix())
