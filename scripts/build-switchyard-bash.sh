#!/usr/bin/env bash
set -euo pipefail

# A disposable opt-in Bash build. Keep its source, license and build log beside
# the binary; never replace the host's Bash or alter the user's shell setting.
version=5.2.37
sha256=9599b22ecd1d5787ad7d3b7bf0c59f312b3396d1e281175dd1f8a4014da621ff
script_dir=$(dirname "$(realpath "$0")")
root=$(mktemp -d -p /tmp/opencode "switchyard-bash-${version}.XXXXXXXX")
archive="$root/bash-${version}.tar.gz"
source="$root/bash-${version}"

curl --proto '=https' --tlsv1.2 -fLsS "https://ftp.gnu.org/gnu/bash/bash-${version}.tar.gz" -o "$archive"
printf '%s  %s\n' "$sha256" "$archive" | sha256sum --check --status
tar --no-same-owner --no-same-permissions -xf "$archive" -C "$root"
patch --batch --forward --fuzz=0 -p1 -d "$source" -i "$script_dir/bash-${version}-switchyard.patch"
(
  cd "$source"
  ./configure --without-bash-malloc --disable-nls --prefix="$root" > "$root/build.log" 2>&1
  make -s -j2 bash >> "$root/build.log" 2>&1
)
install -D -m 0755 "$source/bash" "$root/bin/bash"
printf 'Switchyard Bash: %s/bin/bash\nSource and build log: %s\n' "$root" "$root"
