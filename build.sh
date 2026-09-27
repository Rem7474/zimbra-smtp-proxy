#!/usr/bin/env bash
# Compile l'exe Windows et, si possible, l'installeur.
# Usage : ./build.sh [version]     (Linux, macOS ou Git Bash)
set -euo pipefail
VERSION="${1:-0.0.0}"
cd "$(dirname "$0")"
mkdir -p dist

go test ./...
go run ./tools/genicon
go run github.com/tc-hib/go-winres@v0.3.3 simply --arch amd64 --manifest gui \
  --icon winres/icon.png --product-name "Zimbra SMTP Proxy" \
  --file-description "Passerelle SMTP locale vers Zimbra" \
  --product-version "$VERSION" --file-version "$VERSION" \
  --original-filename zimbra-smtp-proxy.exe
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -H windowsgui -X main.version=$VERSION" \
  -o dist/zimbra-smtp-proxy.exe .
echo "-> dist/zimbra-smtp-proxy.exe"

if command -v iscc >/dev/null; then
  iscc "/DAppVersion=$VERSION" installer/setup.iss
elif command -v docker >/dev/null; then
  chmod a+w dist  # le conteneur tourne sous un utilisateur non-root
  docker run --rm -v "$PWD:/work" amake/innosetup "/DAppVersion=$VERSION" installer/setup.iss
else
  echo "Inno Setup introuvable : installeur non généré." >&2
fi
