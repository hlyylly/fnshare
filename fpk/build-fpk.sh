#!/usr/bin/env bash
# Pack fpk/ into a self-contained fnOS-installable .fpk following the
# real fnOS app layout (verified against conversun's published transmission
# .fpk). Layout:
#
#   <appname>_<version>_<platform>.fpk        (gzipped tar)
#   ├── manifest, ICON.PNG, ICON_256.PNG, fnshare.sc
#   ├── cmd/{common, installer, main, install_init, install_callback,
#   │       uninstall_init, uninstall_callback, upgrade_init,
#   │       upgrade_callback, config_init, config_callback, service-setup}
#   ├── config/{privilege, resource}          (JSON)
#   ├── wizard/config                          (JSON)
#   └── app.tgz                                (tarred-up app/ directory)
#       └── (extracted to ${TRIM_APPDEST}/app/ at install:)
#           ├── docker/docker-compose.yaml
#           ├── images/fnshare-image.tar.gz   (the docker save dump)
#           └── ui/{config, images/{64,256}.png}
set -euo pipefail
cd "$(dirname "$0")"

red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
fail()  { red "ERROR: $*"; exit 1; }

# ----- 1. Validate source tree -----
required_files=(
    manifest ICON.PNG ICON_256.PNG fnshare.sc
    cmd/common cmd/installer cmd/main cmd/service-setup
    cmd/install_init cmd/install_callback
    cmd/uninstall_init cmd/uninstall_callback
    cmd/upgrade_init cmd/upgrade_callback
    cmd/config_init cmd/config_callback
    config/privilege config/resource
    wizard/config
    ui/config ui/images/64.png ui/images/256.png
    app/docker/docker-compose.yaml
    app/ui/config app/ui/images/64.png app/ui/images/256.png
)
for f in "${required_files[@]}"; do
    [ -f "$f" ] || fail "missing required file: $f"
done

require_key() {
    grep -q "^${1}[[:space:]]*=" manifest || fail "manifest missing key: $1"
}
for k in appname version display_name service_port source; do
    require_key "$k"
done

APPNAME=$(awk -F'=' '/^appname/{gsub(/[ \t]/,"",$2); print $2}' manifest)
VERSION=$(awk -F'=' '/^version/{gsub(/[ \t]/,"",$2); print $2}' manifest)
PLATFORM=$(awk -F'=' '/^platform/{gsub(/[ \t]/,"",$2); print $2}' manifest)
PLATFORM=${PLATFORM:-x86}

green "[INFO] packaging ${APPNAME} ${VERSION} (${PLATFORM})"

# ----- 2. Make sure fnshare:latest exists in local docker -----
if ! docker image inspect fnshare:latest >/dev/null 2>&1; then
    if docker image inspect fnshare:dev >/dev/null 2>&1; then
        green "[INFO] tagging fnshare:dev → fnshare:latest"
        docker tag fnshare:dev fnshare:latest
    else
        fail "neither fnshare:latest nor fnshare:dev in local Docker. Build with 'docker build -t fnshare:latest ..' from the repo root first."
    fi
fi

# ----- 3. Stage everything in a temp dir -----
WORK=$(mktemp -d)
PKG="${WORK}/package"
APP_STAGE="${WORK}/app-stage"
mkdir -p "${PKG}" "${APP_STAGE}"

# 3a. Top-level fpk files (everything EXCEPT app/, build script, artifacts)
rsync -a \
    --exclude='build-fpk.sh' \
    --exclude='*.fpk' --exclude='*.tgz' --exclude='*.tar' \
    --exclude='.DS_Store' \
    --exclude='INSTALL.md' \
    --exclude='app/' \
    ./ "${PKG}/"

chmod +x "${PKG}/cmd/"*

# 3b. Stage app/ contents into APP_STAGE (becomes app.tgz)
rsync -a app/ "${APP_STAGE}/"

# 3c. Bundle the docker image into APP_STAGE/images/
mkdir -p "${APP_STAGE}/images"
green "[INFO] saving fnshare:latest into the package…"
docker save fnshare:latest | gzip -9 > "${APP_STAGE}/images/fnshare-image.tar.gz"
IMAGE_SIZE=$(du -h "${APP_STAGE}/images/fnshare-image.tar.gz" | cut -f1)
green "[INFO] image bundled: ${IMAGE_SIZE}"

# ----- 4. Pack app.tgz -----
( cd "${APP_STAGE}" && tar -czf "${PKG}/app.tgz" * )

# ----- 5. Stamp checksum -----
CHECKSUM=$(md5 -q "${PKG}/app.tgz" 2>/dev/null || md5sum "${PKG}/app.tgz" | cut -d' ' -f1)
if sed --version >/dev/null 2>&1; then
    sed -i.bak "s|^checksum.*|checksum              = ${CHECKSUM}|" "${PKG}/manifest"
else
    sed -i '' "s|^checksum.*|checksum              = ${CHECKSUM}|" "${PKG}/manifest"
fi
rm -f "${PKG}/manifest.bak"

# ----- 6. Final .fpk = gzipped tar of the package contents -----
OUT="${PWD}/${APPNAME}_${VERSION}_${PLATFORM}.fpk"
rm -f "${OUT}"
( cd "${PKG}" && tar -czf "${OUT}" * )
rm -rf "${WORK}"

SIZE=$(du -h "${OUT}" | cut -f1)
green "[INFO] built: $(basename "${OUT}") (${SIZE})"
echo "${OUT}"
