#!/usr/bin/env python3
"""npm/Go 의존성 고지 원문을 로컬 입력에서 재현 가능하게 모은다."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
import tempfile
from dataclasses import dataclass, field
from pathlib import Path, PurePosixPath
from typing import Any


LEGAL_FILE_RE = re.compile(r"^(?:license|copying|notice|unlicense)(?:[._-].*)?$", re.IGNORECASE)


@dataclass
class NoticeFile:
    """한 패키지에서 발견한 고지 파일과 표준화된 원문 ID."""

    name: str
    source: str
    notice_id: str | None = None
    decode_warning: bool = False


@dataclass
class Dependency:
    """출력할 의존성의 메타데이터와 고지 상태."""

    ecosystem: str
    name: str
    version: str
    source: str
    declared_license: str
    location: Path | None = None
    missing_reason: str | None = None
    files: list[NoticeFile] = field(default_factory=list)


@dataclass
class RawNotice:
    """동일한 원문을 중복 저장하지 않기 위한 보관 단위."""

    notice_id: str
    digest: str
    sources: list[str]
    text: str
    decode_warning: bool = False


@dataclass
class Collection:
    """npm과 Go 수집 결과 및 미확인 사유."""

    npm: list[Dependency]
    go: list[Dependency]
    raw_notices: list[RawNotice]
    unresolved: list[str]
    npm_reachable: int
    npm_installed: int
    npm_optional_absent: int
    npm_files: int
    go_mode: str
    go_list_error: str | None
    go_declared: int
    go_files: int


def parse_args(argv: list[str]) -> argparse.Namespace:
    """CLI 인자를 정의한다."""

    parser = argparse.ArgumentParser(description="로컬 npm/Go 의존성 고지 원문 생성")
    parser.add_argument("--repo", type=Path, default=Path(__file__).resolve().parents[1], help="저장소 루트")
    parser.add_argument(
        "--output",
        type=Path,
        default=None,
        help="출력 파일(기본: web/public/legal/DEPENDENCY-NOTICES.txt)",
    )
    parser.add_argument(
        "--go-list-json",
        type=Path,
        default=None,
        help="go list -m -json all의 저장 출력(지정하면 명령 실행 안 함)",
    )
    parser.add_argument(
        "--go-mod-json",
        type=Path,
        default=None,
        help="go mod edit -json의 저장 출력(지정하면 fallback 명령 실행 안 함)",
    )
    parser.add_argument("--go-module-cache", type=Path, default=None, help="Go 모듈 캐시 경로")
    parser.add_argument("--check", action="store_true", help="현재 파일이 생성 결과와 같은지 확인")
    return parser.parse_args(argv)


def read_json(path: Path) -> dict[str, Any]:
    """JSON 파일을 읽고 객체인지 확인한다."""

    value = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(value, dict):
        raise ValueError(f"JSON 객체가 필요합니다: {path}")
    return value


def format_metadata(value: Any) -> str:
    """package.json의 license 값을 원문 손실 없이 한 줄로 표시한다."""

    if value is None:
        return "미확인"
    if isinstance(value, str):
        return value
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def package_name_from_lock_path(lock_path: str) -> str:
    """node_modules 경로에서 패키지 이름을 추출한다."""

    parts = PurePosixPath(lock_path).parts
    indices = [index for index, part in enumerate(parts) if part == "node_modules"]
    if not indices:
        return lock_path
    start = indices[-1] + 1
    if start >= len(parts):
        return lock_path
    if parts[start].startswith("@") and start + 1 < len(parts):
        return f"{parts[start]}/{parts[start + 1]}"
    return parts[start]


def resolve_lock_path(packages: dict[str, Any], dependency_name: str, parent: str) -> str | None:
    """npm의 상위 node_modules 탐색 규칙으로 lockfile 경로를 찾는다."""

    current = PurePosixPath(parent)
    candidates: list[str] = []
    while True:
        candidates.append(str(current / "node_modules" / dependency_name))
        if str(current) == ".":
            break
        current = current.parent
    for candidate in candidates:
        if candidate in packages:
            return candidate
    return None


def find_notice_paths(directory: Path) -> list[Path]:
    """패키지 루트의 표준 라이선스/고지 파일만 찾는다."""

    if not directory.is_dir():
        return []
    return sorted(
        (path for path in directory.iterdir() if path.is_file() and LEGAL_FILE_RE.match(path.name)),
        key=lambda path: path.name.casefold(),
    )


def read_notice_text(path: Path) -> tuple[str, bool]:
    """원문 바이트를 UTF-8 텍스트로 읽고 비표준 인코딩 여부를 알린다."""

    data = path.read_bytes()
    text = data.decode("utf-8", errors="replace")
    return text, "\ufffd" in text


def register_notice(
    path: Path,
    source: str,
    raw_by_digest: dict[str, RawNotice],
    raw_notices: list[RawNotice],
) -> tuple[str, bool]:
    """원문을 해시로 deduplicate하고 원문 ID를 반환한다."""

    text, decode_warning = read_notice_text(path)
    digest = hashlib.sha256(path.read_bytes()).hexdigest()
    raw = raw_by_digest.get(digest)
    if raw is None:
        raw = RawNotice(
            notice_id=f"RAW-{len(raw_notices) + 1:04d}",
            digest=digest,
            sources=[source],
            text=text,
            decode_warning=decode_warning,
        )
        raw_by_digest[digest] = raw
        raw_notices.append(raw)
    elif source not in raw.sources:
        raw.sources.append(source)
    return raw.notice_id, decode_warning


def npm_collection(
    repo: Path,
    raw_by_digest: dict[str, RawNotice],
    raw_notices: list[RawNotice],
) -> tuple[list[Dependency], int, int, int, int, list[str]]:
    """lockfile의 production 의존성 그래프와 현재 설치된 원문을 수집한다."""

    web = repo / "web"
    lock_path = web / "package-lock.json"
    lock = read_json(lock_path)
    packages_value = lock.get("packages")
    if not isinstance(packages_value, dict):
        raise ValueError(f"package-lock.json의 packages가 없습니다: {lock_path}")
    packages: dict[str, Any] = packages_value
    root = packages.get("")
    if not isinstance(root, dict):
        raise ValueError(f"package-lock.json의 루트 패키지가 없습니다: {lock_path}")

    stack: list[str] = []
    unresolved: list[str] = []
    for dependency_name in sorted((root.get("dependencies") or {}).keys()):
        lock_entry = resolve_lock_path(packages, dependency_name, ".")
        if lock_entry is None:
            unresolved.append(f"npm {dependency_name}: package-lock 경로 미확인")
        else:
            stack.append(lock_entry)

    reachable: set[str] = set()
    while stack:
        lock_entry = stack.pop()
        if lock_entry in reachable:
            continue
        metadata = packages.get(lock_entry)
        if not isinstance(metadata, dict):
            unresolved.append(f"npm {lock_entry}: package-lock 메타데이터 미확인")
            continue
        # production 루트에서 도달했더라도 lockfile의 dev 표시는 제외한다.
        if metadata.get("dev") is True:
            continue
        reachable.add(lock_entry)
        for field_name in ("dependencies", "optionalDependencies"):
            dependencies = metadata.get(field_name) or {}
            if not isinstance(dependencies, dict):
                continue
            for dependency_name in sorted(dependencies):
                child = resolve_lock_path(packages, dependency_name, lock_entry)
                if child is None:
                    unresolved.append(f"npm {lock_entry} -> {dependency_name}: package-lock 경로 미확인")
                else:
                    stack.append(child)

    dependencies: list[Dependency] = []
    installed_count = 0
    optional_absent = 0
    file_count = 0
    for lock_entry in sorted(reachable):
        metadata = packages[lock_entry]
        package_directory = web / lock_entry
        package_json = package_directory / "package.json"
        installed_metadata: dict[str, Any] = {}
        if package_json.is_file():
            installed_count += 1
            try:
                candidate = read_json(package_json)
                installed_metadata = candidate
            except (OSError, ValueError) as error:
                unresolved.append(f"npm {lock_entry}: package.json 미확인({error})")
        else:
            if metadata.get("optional") is True:
                optional_absent += 1
            else:
                unresolved.append(f"npm {lock_entry}: 현재 설치 트리의 package.json 미확인")

        package_name = str(installed_metadata.get("name") or package_name_from_lock_path(lock_entry))
        version = str(metadata.get("version") or installed_metadata.get("version") or "미확인")
        declared = metadata.get("license")
        if declared is None:
            declared = installed_metadata.get("license")
        if declared is None:
            declared = installed_metadata.get("licenses")
        source = f"web/package-lock.json#packages[\"{lock_entry}\"]"
        dependency = Dependency(
            ecosystem="npm",
            name=package_name,
            version=version,
            source=source,
            declared_license=format_metadata(declared),
            location=package_directory if package_directory.is_dir() else None,
        )
        if not package_directory.is_dir():
            dependency.missing_reason = "현재 환경에서 선택적 패키지가 설치되지 않음"
        notice_paths = find_notice_paths(package_directory)
        for notice_path in notice_paths:
            source_label = f"web/{notice_path.relative_to(web).as_posix()}"
            notice_id, decode_warning = register_notice(notice_path, source_label, raw_by_digest, raw_notices)
            dependency.files.append(
                NoticeFile(
                    name=notice_path.name,
                    source=source_label,
                    notice_id=notice_id,
                    decode_warning=decode_warning,
                )
            )
        file_count += len(notice_paths)
        if not notice_paths:
            reason = dependency.missing_reason or "패키지 루트에 표준 고지 파일이 없음"
            unresolved.append(f"npm {package_name}@{version}: 원문 미확인({reason}; 선언 license: {dependency.declared_license})")
        dependencies.append(dependency)
    return dependencies, len(reachable), installed_count, optional_absent, file_count, unresolved


def command_environment() -> dict[str, str]:
    """네트워크 없이 Go 명령을 실행할 임시 환경을 만든다."""

    environment = os.environ.copy()
    environment["GOENV"] = "off"
    environment["GOPROXY"] = "off"
    environment["GOSUMDB"] = "off"
    environment["GOTOOLCHAIN"] = "local"
    environment.setdefault("GOCACHE", str(Path(tempfile.gettempdir()) / "artex-go-build-cache"))
    go_tmpdir = Path(tempfile.gettempdir()) / "artex-go-tmp"
    go_tmpdir.mkdir(parents=True, exist_ok=True)
    # Go 환경 설정 파일에 남아 있는 경로 대신 매 실행의 임시 작업 디렉터리를 쓴다.
    environment["GOTMPDIR"] = str(go_tmpdir)
    return environment


def run_go_command(repo: Path, args: list[str], environment: dict[str, str]) -> tuple[int, str, str]:
    """Go 명령을 실행하되 네트워크가 켜지지 않도록 제한한다."""

    try:
        process = subprocess.run(
            args,
            cwd=repo,
            env=environment,
            text=True,
            encoding="utf-8",
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            timeout=120,
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        return 1, "", str(error)
    return process.returncode, process.stdout, process.stderr


def parse_json_stream(text: str) -> list[dict[str, Any]]:
    """go list의 연속 JSON 객체 출력을 파싱한다."""

    decoder = json.JSONDecoder()
    cursor = 0
    objects: list[dict[str, Any]] = []
    while cursor < len(text):
        while cursor < len(text) and text[cursor].isspace():
            cursor += 1
        if cursor >= len(text):
            break
        value, cursor = decoder.raw_decode(text, cursor)
        if isinstance(value, dict):
            objects.append(value)
    return objects


def go_module_cache(repo: Path, environment: dict[str, str], requested: Path | None) -> Path:
    """현재 Go 모듈 캐시 경로를 찾는다."""

    if requested is not None:
        return requested
    if environment.get("GOMODCACHE"):
        return Path(environment["GOMODCACHE"])
    code, stdout, _ = run_go_command(repo, ["go", "env", "GOMODCACHE"], environment)
    if code == 0 and stdout.strip():
        return Path(stdout.strip())
    return Path.home() / "go" / "pkg" / "mod"


def escape_module_path(module_path: str) -> str:
    """Go 모듈 캐시의 대소문자 escape 규칙을 적용한다."""

    escaped: list[str] = []
    for character in module_path:
        if "A" <= character <= "Z":
            escaped.append("!" + character.lower())
        elif character == "!":
            escaped.append("!!")
        else:
            escaped.append(character)
    return "".join(escaped)


def fallback_go_modules(repo: Path, mod_json_path: Path | None, environment: dict[str, str]) -> tuple[list[dict[str, Any]], str | None]:
    """go list가 캐시 부족으로 실패할 때 go.mod 선언을 보조 입력으로 읽는다."""

    if mod_json_path is not None:
        try:
            value = read_json(mod_json_path)
        except (OSError, ValueError) as error:
            return [], f"go mod edit JSON 읽기 실패: {error}"
    else:
        code, stdout, stderr = run_go_command(repo, ["go", "mod", "edit", "-json"], environment)
        if code != 0:
            return [], f"go mod edit -json 실패: {summarize_command_error(stderr)}"
        try:
            value = json.loads(stdout)
        except json.JSONDecodeError as error:
            return [], f"go mod edit JSON 파싱 실패: {error}"
    requires = value.get("Require") or []
    if not isinstance(requires, list):
        return [], "go.mod Require 배열 미확인"
    modules = [entry for entry in requires if isinstance(entry, dict) and entry.get("Path") and entry.get("Version")]
    return modules, None


def summarize_command_error(stderr: str) -> str:
    """환경별 절대 경로를 출력하지 않고 명령 실패를 짧게 표시한다."""

    lines = [line.strip() for line in stderr.splitlines() if line.strip()]
    if not lines:
        return "stderr 없음"
    if len(lines) > 4:
        return " / ".join(lines[:4]) + f" / ... {len(lines) - 4}줄 생략"
    return " / ".join(lines)


def go_collection(
    repo: Path,
    raw_by_digest: dict[str, RawNotice],
    raw_notices: list[RawNotice],
    go_list_json: Path | None,
    go_mod_json: Path | None,
    requested_cache: Path | None,
) -> tuple[list[Dependency], str, str | None, int, int, list[str]]:
    """Go 모듈 원문을 go list 우선, go.mod fallback 순서로 수집한다."""

    environment = command_environment()
    go_list_error: str | None = None
    if go_list_json is not None:
        try:
            stream = go_list_json.read_text(encoding="utf-8")
            modules = parse_json_stream(stream)
            mode = "go list 입력 파일"
        except (OSError, ValueError) as error:
            modules = []
            go_list_error = f"go list 입력 파일 읽기 실패: {error}"
            mode = "go.mod fallback"
    else:
        code, stdout, stderr = run_go_command(repo, ["go", "list", "-m", "-json", "all"], environment)
        if code == 0:
            try:
                modules = parse_json_stream(stdout)
                mode = "go list -m -json all"
            except ValueError as error:
                modules = []
                mode = "go.mod fallback"
                go_list_error = f"go list JSON 파싱 실패: {error}"
        else:
            modules = []
            mode = "go.mod fallback"
            go_list_error = f"go list -m -json all 실패: {summarize_command_error(stderr)}"

    if not modules:
        modules, fallback_error = fallback_go_modules(repo, go_mod_json, environment)
        mode = "go.mod fallback"
        if fallback_error:
            go_list_error = "; ".join(value for value in (go_list_error, fallback_error) if value)

    module_cache = go_module_cache(repo, environment, requested_cache)
    dependencies: list[Dependency] = []
    unresolved: list[str] = []
    file_count = 0
    for module in sorted(modules, key=lambda item: (str(item.get("Path", "")), str(item.get("Version", "")))):
        if module.get("Main") is True:
            continue
        module_path = str(module.get("Path") or "미확인")
        version = str(module.get("Version") or "미확인")
        replacement = module.get("Replace") if isinstance(module.get("Replace"), dict) else None
        resolved_path = str(replacement.get("Path")) if replacement and replacement.get("Path") else module_path
        resolved_version = str(replacement.get("Version")) if replacement and replacement.get("Version") else version
        location_value = module.get("Dir")
        if replacement and replacement.get("Dir"):
            location_value = replacement["Dir"]
        location = Path(str(location_value)) if location_value else module_cache / f"{escape_module_path(resolved_path)}@{resolved_version}"
        source = f"{mode}:{module_path}@{version}"
        dependency = Dependency(
            ecosystem="go",
            name=module_path,
            version=version,
            source=source,
            declared_license="미확인",
            location=location if location.is_dir() else None,
        )
        notice_paths = find_notice_paths(location)
        for notice_path in notice_paths:
            source_label = f"{module_path}@{version}/{notice_path.name}"
            notice_id, decode_warning = register_notice(notice_path, source_label, raw_by_digest, raw_notices)
            dependency.files.append(
                NoticeFile(
                    name=notice_path.name,
                    source=source_label,
                    notice_id=notice_id,
                    decode_warning=decode_warning,
                )
            )
        file_count += len(notice_paths)
        if not notice_paths:
            if dependency.location is None:
                dependency.missing_reason = "Go 모듈 캐시의 디렉터리 미확인"
            else:
                dependency.missing_reason = "모듈 루트에 표준 고지 파일이 없음"
            unresolved.append(f"go {module_path}@{version}: 원문 미확인({dependency.missing_reason})")
        dependencies.append(dependency)
    return dependencies, mode, go_list_error, len(modules), file_count, unresolved


def dependency_sort_key(dependency: Dependency) -> tuple[str, str, str]:
    """출력 순서를 ecosystem/name/version으로 고정한다."""

    return dependency.ecosystem, dependency.name, dependency.version


def render_dependency(dependency: Dependency) -> list[str]:
    """의존성 하나의 메타데이터와 원문 링크를 렌더링한다."""

    lines = [f"### {dependency.name}@{dependency.version}", f"- 출처: `{dependency.source}`"]
    lines.append(f"- 선언된 license 메타데이터: `{dependency.declared_license}`")
    if dependency.missing_reason:
        lines.append(f"- 설치 상태: {dependency.missing_reason}")
    if dependency.files:
        for notice_file in dependency.files:
            warning = " (UTF-8 외 인코딩 치환 표시 포함)" if notice_file.decode_warning else ""
            lines.append(f"- 원문: `{notice_file.name}` → `{notice_file.notice_id}`{warning}")
    else:
        lines.append("- 원문: **미확인** (수집된 표준 파일 없음)")
    lines.append("")
    return lines


def render_raw_notice(raw: RawNotice) -> list[str]:
    """원문을 별도 섹션에 바이트 순서대로 렌더링한다."""

    lines = [f"### {raw.notice_id}", f"- SHA-256: `{raw.digest}`", "- 출처:"]
    lines.extend(f"  - `{source}`" for source in sorted(raw.sources))
    if raw.decode_warning:
        lines.append("- 인코딩: UTF-8이 아닌 바이트가 있어 치환 문자를 사용함")
    lines.append("- 원문 시작")
    lines.append("----- BEGIN ORIGINAL NOTICE -----")
    lines.append(raw.text)
    if not raw.text.endswith("\n"):
        lines.append("")
    lines.append("----- END ORIGINAL NOTICE -----")
    lines.append("")
    return lines


def build_document(collection: Collection) -> str:
    """수집 결과를 deterministic한 공개 고지 문서로 만든다."""

    lines: list[str] = [
        "# ARTEX 의존성 라이선스·고지 원문",
        "",
        "이 파일은 현재 로컬에 설치된 npm production 의존성과 Go 의존성에서 수집한 메타데이터 및 원문입니다.",
        "원문은 패키지 배포본에서 읽었고, 동일한 원문은 RAW ID로 한 번만 보관했습니다.",
        "라이선스 호환성, 배포 허용 여부 또는 법률적 의미를 판단하는 문서가 아닙니다.",
        "미확인 항목은 임의로 추정하지 않고 그대로 표시했습니다.",
        "",
        "## 수집 기준",
        "",
        f"- npm production lockfile 도달 항목: {collection.npm_reachable}개",
        f"- npm 현재 설치 package.json 확인: {collection.npm_installed}개",
        f"- npm 선택적 lockfile 항목 중 현재 미설치: {collection.npm_optional_absent}개",
        f"- npm 원문 파일: {collection.npm_files}개",
        f"- Go 모듈 수집 방식: `{collection.go_mode}`",
        f"- Go 모듈 선언/수집 항목: {collection.go_declared}개",
        f"- Go 원문 파일: {collection.go_files}개",
        f"- 원문 중복 제거 후 RAW 블록: {len(collection.raw_notices)}개",
        "",
        "생성기는 `GOENV=off`, `GOPROXY=off`, `GOSUMDB=off`와 임시 GOCACHE/GOTMPDIR로 Go 명령을 실행합니다.",
        "npm install이나 네트워크 요청은 수행하지 않으며, package-lock.json과 현재 node_modules를 읽습니다.",
        "",
        "## npm production dependencies",
        "",
    ]
    for dependency in sorted(collection.npm, key=dependency_sort_key):
        lines.extend(render_dependency(dependency))

    lines.extend(["## Go dependencies", ""])
    if collection.go_list_error:
        lines.extend(
            [
                "### go list 상태",
                f"- **미확인**: {collection.go_list_error}",
                "- go list가 캐시 부족으로 실패한 경우 go.mod 선언을 fallback으로 표시했습니다.",
                "",
            ]
        )
    for dependency in sorted(collection.go, key=dependency_sort_key):
        lines.extend(render_dependency(dependency))

    if collection.unresolved:
        lines.extend(["## 미확인 항목", "", "원문 또는 의존성 경로를 확인하지 못한 항목입니다.", ""])
        lines.extend(f"- {item}" for item in sorted(set(collection.unresolved)))
        lines.append("")

    lines.extend(["## 원문 보관", ""])
    for raw in collection.raw_notices:
        lines.extend(render_raw_notice(raw))
    return "\n".join(lines).rstrip("\n") + "\n"


def collect(repo: Path, args: argparse.Namespace) -> Collection:
    """두 생태계의 수집을 실행한다."""

    raw_by_digest: dict[str, RawNotice] = {}
    raw_notices: list[RawNotice] = []
    npm, reachable, installed, optional_absent, npm_files, npm_unresolved = npm_collection(
        repo, raw_by_digest, raw_notices
    )
    go, go_mode, go_error, go_declared, go_files, go_unresolved = go_collection(
        repo,
        raw_by_digest,
        raw_notices,
        args.go_list_json,
        args.go_mod_json,
        args.go_module_cache,
    )
    return Collection(
        npm=npm,
        go=go,
        raw_notices=raw_notices,
        unresolved=npm_unresolved + go_unresolved,
        npm_reachable=reachable,
        npm_installed=installed,
        npm_optional_absent=optional_absent,
        npm_files=npm_files,
        go_mode=go_mode,
        go_list_error=go_error,
        go_declared=go_declared,
        go_files=go_files,
    )


def main(argv: list[str] | None = None) -> int:
    """생성 또는 check 모드를 실행한다."""

    args = parse_args(argv if argv is not None else sys.argv[1:])
    repo = args.repo.resolve()
    output = (args.output or repo / "web/public/legal/DEPENDENCY-NOTICES.txt").resolve()
    try:
        document = build_document(collect(repo, args))
    except (OSError, ValueError, json.JSONDecodeError) as error:
        print(f"라이선스 고지 생성 실패: {error}", file=sys.stderr)
        return 2
    if args.check:
        try:
            # CRLF 원문을 LF로 바꾸면 check가 원문 바이트를 거짓으로 보고한다.
            with output.open("r", encoding="utf-8", newline="") as stream:
                existing = stream.read()
        except OSError as error:
            print(f"check 실패: {output} 읽기 실패: {error}", file=sys.stderr)
            return 1
        if existing != document:
            print(f"check 실패: {output}가 현재 로컬 입력과 다릅니다", file=sys.stderr)
            return 1
        print(f"check 통과: {output}")
        return 0
    output.parent.mkdir(parents=True, exist_ok=True)
    with output.open("w", encoding="utf-8", newline="") as stream:
        stream.write(document)
    print(f"생성 완료: {output} ({len(document.encode('utf-8'))} bytes)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
