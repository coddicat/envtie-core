#!/usr/bin/env sh
# Builds release binaries named <name>_<tag>_<os>_<arch>[.exe] plus SHA256SUMS.txt.
# Flags must stay identical to Dockerfile.runner so adapter pins match the image.
# Usage: scripts/release-binaries.sh <tag> <outdir> [os/arch ...]
set -eu
tag=${1:?tag required}
out=${2:?output directory required}
shift 2
targets=${*:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64}
mkdir -p "$out"
out=$(CDPATH= cd -- "$out" && pwd)
for target in $targets; do
  os=${target%/*}; arch=${target#*/}; ext=""; [ "$os" = windows ] && ext=".exe"
  for name in envtie adapter-neon adapter-upstash adapter-github adapter-stripe; do
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -buildvcs=false \
      -ldflags="-s -w -X main.version=${tag}" \
      -o "$out/${name}_${tag}_${os}_${arch}${ext}" "./cmd/${name}"
  done
done
cd "$out" && sha256sum -- *_"${tag}"_* > SHA256SUMS.txt && cat SHA256SUMS.txt
repository=${GITHUB_REPOSITORY:-coddicat/envtie-core}
base_url=${ENVTIE_RELEASE_URL:-https://github.com/$repository/releases/download/$tag}
python3 - "$out" "$tag" "$base_url" <<'PY'
import hashlib, json, os, re, sys
out, tag, base_url = sys.argv[1], sys.argv[2], sys.argv[3]
pattern = re.compile(r"^adapter-(neon|upstash|github|stripe)_" + re.escape(tag) + r"_(linux|darwin|windows)_(amd64|arm64)(?:\.exe)?$")
catalog = {"release": tag, "base_url": base_url.rstrip("/"), "adapters": {}}
for name in sorted(os.listdir(out)):
    match = pattern.match(name)
    if not match:
        continue
    digest = hashlib.sha256(open(os.path.join(out, name), "rb").read()).hexdigest()
    catalog["adapters"].setdefault(match.group(1) + "@1", {})[f"{match.group(2)}/{match.group(3)}"] = {
        "sha256": digest,
        "size": os.path.getsize(os.path.join(out, name)),
    }
path = os.path.join(out, "adapters.json")
with open(path, "w", encoding="utf-8", newline="\n") as fh:
    json.dump(catalog, fh, indent=2)
    fh.write("\n")
print(f"wrote {path}")
PY
