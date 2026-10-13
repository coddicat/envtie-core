#!/usr/bin/env sh
set -eu
cd "$(dirname "$0")/.."
mkdir -p dist
for name in envtie adapter-neon adapter-upstash adapter-github adapter-stripe; do
  go build -trimpath -buildvcs=false -ldflags '-buildid=' -o "dist/$name" "./cmd/$name"
  sha256sum "dist/$name"
done
arch="$(go env GOARCH)"
os="$(go env GOOS)"
python3 - "dist" "$os" "$arch" <<'PY'
import hashlib, json, os, sys
out, goos, arch = sys.argv[1], sys.argv[2], sys.argv[3]
catalog = {"release": "local", "adapters": {}}
for name in ("neon", "upstash", "github", "stripe"):
    path = os.path.join(out, f"adapter-{name}")
    digest = hashlib.sha256(open(path, "rb").read()).hexdigest()
    adapter = f"{name}@1"
    catalog["adapters"][adapter] = {f"{goos}/{arch}": {
        "sha256": digest,
        "size": os.path.getsize(path),
        "url": f"/v1/adapter-artifacts/{adapter}/{goos}/{arch}",
    }}
dest = os.path.join(out, "adapters.json")
with open(dest, "w", encoding="utf-8", newline="\n") as fh:
    json.dump(catalog, fh, indent=2)
    fh.write("\n")
print(f"wrote {dest}")
PY
