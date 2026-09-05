# Third-party components

Spoticonn runs these unmodified upstream executables as separate processes:

| Component | Version | Source and license |
| --- | --- | --- |
| go-librespot | v0.9.0 | https://github.com/devgianlu/go-librespot/tree/v0.9.0 — GPL-3.0 |
| cliairplay | v0.5.3 | https://github.com/music-assistant/airplay-cli/tree/v0.5.3 — GPL-3.0; see its bundled notices for incorporated libraries |

Their license texts and the cliairplay distribution's third-party notice are in
`third_party/` and included at `/usr/share/licenses/spoticonn/` in the image.
Redistributors must preserve these notices and meet applicable source distribution
requirements, including cliairplay's GPL requirements. Exact upstream versions are
pinned; `scripts/fetch-engines.sh` verifies published release digests.

The Go and npm dependency trees are pinned by `go.sum` and `web/package-lock.json`.
Spotify and AirPlay are names belonging to their respective owners. This project
uses community implementations and is not affiliated with Spotify or Apple.
