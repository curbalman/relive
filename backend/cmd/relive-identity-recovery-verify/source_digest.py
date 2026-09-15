#!/usr/bin/env python3
"""source_digest.py

Single source of truth for computing the source digest of Relive backend code.
Used by record_test_evidence.sh and the build/verification process.
"""

import argparse
import hashlib
import json
import os
import subprocess
import sys


def find_repo_root(hint: str = None) -> str:
    if hint and os.path.isdir(hint):
        return os.path.abspath(hint)
    script_dir = os.path.dirname(os.path.abspath(__file__))
    # script is in backend/cmd/relive-identity-recovery-verify -> 3 levels up to repo root
    cand = os.path.abspath(os.path.join(script_dir, "../../.."))
    if os.path.isfile(os.path.join(cand, "backend", "go.mod")):
        return cand
    cwd = os.getcwd()
    if os.path.isfile(os.path.join(cwd, "backend", "go.mod")):
        return cwd
    if os.path.isfile(os.path.join(cwd, "go.mod")):
        return os.path.abspath(os.path.join(cwd, ".."))
    raise RuntimeError("Cannot determine repository root")


def get_embed_files(repo_root: str) -> list:
    backend_dir = os.path.join(repo_root, "backend")
    if not os.path.isdir(backend_dir):
        raise RuntimeError(f"Backend directory not found: {backend_dir}")

    # Run go list -deps -test -json ./... to find all embedded files declared by packages
    env = os.environ.copy()
    if "DEVELOPER_DIR" not in env and os.path.isdir("/Library/Developer/CommandLineTools"):
        env["DEVELOPER_DIR"] = "/Library/Developer/CommandLineTools"

    cmd = ["go", "list", "-deps", "-test", "-json", "./..."]
    proc = subprocess.run(cmd, cwd=backend_dir, capture_output=True, text=True, env=env)
    if proc.returncode != 0:
        raise RuntimeError(f"go list failed (code {proc.returncode}): {proc.stderr.strip()}")

    decoder = json.JSONDecoder()
    content = proc.stdout
    idx = 0
    embed_paths = []
    while idx < len(content):
        content_slice = content[idx:].lstrip()
        if not content_slice:
            break
        idx += len(content[idx:]) - len(content_slice)
        obj, end = decoder.raw_decode(content[idx:])
        idx += end

        mod = obj.get("Module")
        import_path = obj.get("ImportPath", "")
        # Only check first-party packages
        is_first_party = (mod and mod.get("Path") == "github.com/davidhoo/relive") or import_path.startswith("github.com/davidhoo/relive")
        if not is_first_party:
            continue

        pkg_dir = obj.get("Dir", "")
        for key in ["EmbedFiles", "TestEmbedFiles", "XTestEmbedFiles"]:
            for f in obj.get(key, []):
                full_path = os.path.normpath(os.path.join(pkg_dir, f))
                rel = os.path.relpath(full_path, repo_root)
                embed_paths.append(rel)

    return embed_paths


def collect_source_files(repo_root: str) -> list:
    files = set()
    backend_dir = os.path.join(repo_root, "backend")

    # 1. First-party Go sources and tests in cmd, internal, pkg
    for subdir in ["cmd", "internal", "pkg"]:
        dir_path = os.path.join(backend_dir, subdir)
        if not os.path.isdir(dir_path):
            continue
        for root, dirs, filenames in os.walk(dir_path):
            # Exclude cache or hidden directories
            dirs[:] = [d for d in dirs if not d.startswith(".") and d != "testdata_ignore"]
            for f in filenames:
                if f.startswith(".") or f.endswith("~"):
                    continue
                # Go files, C/C++ files, assembly and headers
                ext = os.path.splitext(f)[1].lower()
                if ext in [".go", ".c", ".cpp", ".cc", ".h", ".hpp", ".s"]:
                    full_p = os.path.join(root, f)
                    rel = os.path.relpath(full_p, repo_root)
                    files.add(rel)

    # 2. go.mod and go.sum
    for top_file in ["go.mod", "go.sum"]:
        p = os.path.join(backend_dir, top_file)
        if os.path.isfile(p):
            files.add(os.path.relpath(p, repo_root))
        else:
            raise RuntimeError(f"Missing required file: {p}")

    # 3. Embed files
    embed_files = get_embed_files(repo_root)
    for ef in embed_files:
        full_p = os.path.join(repo_root, ef)
        if not os.path.isfile(full_p):
            raise RuntimeError(f"Embedded file not found: {ef}")
        files.add(ef)

    # 4. Scripts themselves
    script_rel = os.path.relpath(os.path.abspath(__file__), repo_root)
    files.add(script_rel)

    rec_rel = "backend/cmd/relive-identity-recovery-verify/record_test_evidence.sh"
    if os.path.isfile(os.path.join(repo_root, rec_rel)):
        files.add(rec_rel)

    # Filter out any accidentally matched output files, logs, or DBs
    excluded_extensions = {".db", ".db-shm", ".db-wal", ".log", ".jsonl", ".bin", ".out", ".exe"}
    result = []
    for f in files:
        f_norm = f.replace("\\", "/")
        ext = os.path.splitext(f_norm)[1].lower()
        if ext in excluded_extensions:
            continue
        if "test-evidence.json" in f_norm or "verify-report" in f_norm:
            continue
        result.append(f_norm)

    result.sort()
    return result


def compute_digest(repo_root: str, files: list) -> tuple:
    overall_hasher = hashlib.sha256()
    file_records = []

    for rel_path in files:
        full_path = os.path.join(repo_root, rel_path)
        try:
            with open(full_path, "rb") as f:
                content = f.read()
        except Exception as e:
            raise RuntimeError(f"Failed to read {rel_path}: {e}")

        file_sha = hashlib.sha256(content).hexdigest()
        file_records.append({
            "path": rel_path,
            "size": len(content),
            "sha256": file_sha,
        })

        header = f"{rel_path}:{len(content)}\n".encode("utf-8")
        overall_hasher.update(header)
        overall_hasher.update(content)

    total_digest = overall_hasher.hexdigest()
    return total_digest, file_records


def main():
    parser = argparse.ArgumentParser(description="Compute Relive source digest.")
    parser.add_argument("--repo-root", help="Path to repository root")
    parser.add_argument("--digest-only", action="store_true", help="Output only the 64-char hex digest")
    parser.add_argument("--json", action="store_true", help="Output JSON manifest with file list")
    parser.add_argument("--write-manifest", help="Write JSON manifest to specified file path")
    args = parser.parse_args()

    try:
        repo_root = find_repo_root(args.repo_root)
        files = collect_source_files(repo_root)
        digest, records = compute_digest(repo_root, files)

        if args.write_manifest:
            out_obj = {
                "source_digest": digest,
                "file_count": len(records),
                "files": records,
            }
            with open(args.write_manifest, "w", encoding="utf-8") as f:
                json.dump(out_obj, f, indent=2)

        if args.digest_only:
            print(digest)
        elif args.json:
            out_obj = {
                "source_digest": digest,
                "file_count": len(records),
                "files": records,
            }
            print(json.dumps(out_obj, indent=2))
        else:
            print(f"Relive Source Digest: {digest}")
            print(f"Total files: {len(records)}")
            for r in records[:5]:
                print(f"  {r['path']} ({r['size']} bytes) - {r['sha256'][:12]}")
            if len(records) > 5:
                print(f"  ... and {len(records) - 5} more files")

    except Exception as e:
        sys.stderr.write(f"source_digest.py: error: {e}\n")
        sys.exit(1)


if __name__ == "__main__":
    main()
