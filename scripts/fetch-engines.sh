#!/bin/sh
set -eu
arch=${1:?amd64 or arm64 required}
destination=${2:?destination required}
case "$arch" in
  amd64)
    spotify_asset=go-librespot_linux_x86_64.tar.gz
    spotify_hash=87b27ce57cc7871bad6ffac8acba7e2a89c72a7e6c7990b88bec404f449f381e
    airplay_asset=cliairplay-linux-x86_64
    airplay_hash=fd6fa451cdfd0c83c502e8cdfc7e73b24a9553e7058508b5e8c69cdd1dd621dd
    ;;
  arm64)
    spotify_asset=go-librespot_linux_arm64.tar.gz
    spotify_hash=79b80bb3723b7973165d2d94c428676b8582780aeca7c54694589206ab741e91
    airplay_asset=cliairplay-linux-aarch64
    airplay_hash=461938ebb9a23ebb8aa69d956e955c704e5c3cbb1750f039922171479a6e4918
    ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac
mkdir -p "$destination"
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
curl --fail --location --retry 3 --proto '=https' "https://github.com/devgianlu/go-librespot/releases/download/v0.9.0/$spotify_asset" -o "$temporary/spotify.tar.gz"
curl --fail --location --retry 3 --proto '=https' "https://github.com/music-assistant/airplay-cli/releases/download/v0.5.3/$airplay_asset" -o "$temporary/cliairplay"
printf '%s  %s\n' "$spotify_hash" "$temporary/spotify.tar.gz" "$airplay_hash" "$temporary/cliairplay" | sha256sum -c -
tar -xzf "$temporary/spotify.tar.gz" -C "$temporary" go-librespot
install -m 0755 "$temporary/go-librespot" "$destination/go-librespot"
install -m 0755 "$temporary/cliairplay" "$destination/cliairplay"
