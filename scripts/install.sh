#!/usr/bin/env bash
# Installs the frostfall release binary matching the action's own tag.
# Called by action.yml with FROSTFALL_ACTION_REF set to the ref the user
# pinned (v1.2.3 or a major tag like v1). Major tags resolve to the newest
# release under that major - never to a different major - and every download
# is sha256-verified against the release's checksums.txt.
set -euo pipefail

REPO="aquia-inc/frostfall"
REF="${FROSTFALL_ACTION_REF:?FROSTFALL_ACTION_REF not set}"

case "$(uname -s)" in
  Linux|Darwin) OS="$(uname -s)" ;;
  *) echo "unsupported OS: $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64)  ARCH=x86_64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac

# tag_from_json extracts tag_name values from GitHub API JSON (portable: no
# GNU grep -P, works with BSD userland on macOS runners).
tags_from_json() {
  sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p'
}

if [[ "$REF" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  TAG="$REF"
elif [[ "$REF" =~ ^v[0-9]+$ ]]; then
  # Major tag: newest release under the SAME major only.
  TAG=$(curl -sfL "https://api.github.com/repos/${REPO}/releases?per_page=100" \
        | tags_from_json | grep "^${REF}\." | head -n1 || true)
  if [ -z "$TAG" ]; then
    echo "no release found under major ${REF}" >&2
    exit 1
  fi
else
  # A SHA (or branch) pin cannot be resolved to a release safely; installing
  # "latest" would silently ignore the pin. Refuse instead.
  echo "cannot resolve action ref '${REF}' to a release; pin a version tag (v1 or v1.2.3)" >&2
  exit 1
fi

ASSET="frostfall_${OS}_${ARCH}.tar.gz"
BASE="https://github.com/${REPO}/releases/download/${TAG}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

curl -sfL "${BASE}/${ASSET}" -o "${TMP}/${ASSET}"
curl -sfL "${BASE}/checksums.txt" -o "${TMP}/checksums.txt"
(
  cd "$TMP"
  grep " ${ASSET}\$" checksums.txt | if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c -
  else
    shasum -a 256 -c -
  fi
)

DEST="${RUNNER_TEMP:-/tmp}/frostfall"
mkdir -p "$DEST"
tar -xzf "${TMP}/${ASSET}" -C "$DEST" frostfall
echo "$DEST" >> "$GITHUB_PATH"
"$DEST/frostfall" --version
