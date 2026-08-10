#!/usr/bin/env python3
"""Create a deterministic gzip-compressed tar archive from one package tree."""

from __future__ import annotations

import argparse
import gzip
import tarfile
from pathlib import Path


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--name", required=True)
    parser.add_argument("--epoch", type=int, required=True)
    args = parser.parse_args()

    source = args.source.resolve()
    if not source.is_dir() or source.is_symlink():
        parser.error(f"source must be a real directory: {source}")
    args.output.parent.mkdir(parents=True, exist_ok=True)

    with args.output.open("wb") as destination:
        with gzip.GzipFile(fileobj=destination, mode="wb", mtime=args.epoch) as compressed:
            with tarfile.open(fileobj=compressed, mode="w") as archive:
                members = sorted(source.rglob("*"), key=lambda path: path.relative_to(source).as_posix())
                for path in members:
                    relative = path.relative_to(source).as_posix()
                    info = archive.gettarinfo(str(path), arcname=f"{args.name}/{relative}")
                    info.uid = 0
                    info.gid = 0
                    info.uname = ""
                    info.gname = ""
                    info.mtime = args.epoch
                    if info.isreg():
                        with path.open("rb") as source_file:
                            archive.addfile(info, source_file)
                    else:
                        archive.addfile(info)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
