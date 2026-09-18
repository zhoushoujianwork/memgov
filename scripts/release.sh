#!/usr/bin/env bash
set -euo pipefail
release_version="${1:-2.0.0-rc1}"
if [[ ! "$release_version" =~ ^[0-9A-Za-z._-]+$ ]]; then
  echo 'Invalid release version' >&2
  exit 2
fi
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
make web-build
mkdir -p dist
for release_os in darwin linux; do
  for release_arch in arm64 amd64; do
    package="memgov-${release_version}-${release_os}-${release_arch}"
    rm -rf "dist/$package"
    mkdir -p "dist/$package"
    CGO_ENABLED=0 GOOS="$release_os" GOARCH="$release_arch" go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X github.com/zhoushoujianwork/memgov/internal/cli.Version=$release_version" \
      -o "dist/$package/memgov" ./cmd/memgov
    cp README.md "dist/$package/README.md"
    cp INSTALL.md "dist/$package/INSTALL.md"
    cp config.local.yaml.example "dist/$package/config.local.yaml.example"
    cp -R docs "dist/$package/docs"
    tar -czf "dist/$package.tar.gz" -C "dist/$package" memgov README.md INSTALL.md config.local.yaml.example docs
  done
done
python3 - "$release_version" <<'PY'
import hashlib,pathlib,sys
root=pathlib.Path('dist')
archives=sorted(root.glob(f'memgov-{sys.argv[1]}-*.tar.gz'))
(root/'SHA256SUMS').write_text(''.join(f'{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n' for p in archives))
print('\n'.join(str(p) for p in archives))
PY
