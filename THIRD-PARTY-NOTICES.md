# Third-party notices

BigBadRescue's own code is released under the MIT licence (see `LICENSE`). It
uses, or ships with, the following software, each under its own licence.

## FFmpeg (bundled in BigBadRescue.app)

`BigBadRescue.app/Contents/Resources/bin/ffmpeg-arm64` and `ffmpeg-x86_64`
are unmodified FFmpeg 6.0 builds for macOS, taken from the
[ffmpeg-static](https://github.com/eugeneware/ffmpeg-static) project
(release `b6.0`). These builds include GPL-licensed components (such as
LAME for MP3), so the bundled binaries are distributed under the
**GNU General Public License, version 3**.

- FFmpeg: https://ffmpeg.org (licence details: https://ffmpeg.org/legal.html)
- FFmpeg 6.0 source code: https://ffmpeg.org/releases/ffmpeg-6.0.tar.xz
- Build recipes for these binaries: https://github.com/eugeneware/ffmpeg-static
- GPL v3 text: https://www.gnu.org/licenses/gpl-3.0.html

BigBadRescue runs FFmpeg as a separate program and does not link to it.
If you build from source and FFmpeg is not bundled, BigBadRescue uses the
copy installed on your Mac (for example from Homebrew).

## Go libraries (compiled into the BigBadRescue program)

| Library | Version | Licence |
|---|---|---|
| github.com/modelcontextprotocol/go-sdk | v1.8.0 | MIT / Apache-2.0 (the project is moving to Apache-2.0) |
| github.com/google/jsonschema-go | v0.4.3 | MIT |
| github.com/segmentio/encoding | v0.5.4 | MIT |
| github.com/segmentio/asm | v1.1.3 | MIT |
| github.com/yosida95/uritemplate/v3 | v3.0.2 | BSD-3-Clause |
| golang.org/x/oauth2 | v0.35.0 | BSD-3-Clause |
| golang.org/x/sync | v0.20.0 | BSD-3-Clause |
| golang.org/x/sys | v0.41.0 | BSD-3-Clause |
| golang.org/x/time | v0.15.0 | BSD-3-Clause |
| The Go standard library | 1.25 | BSD-3-Clause |

Each library's full licence text is in its source repository and in the Go
module cache (`go env GOMODCACHE`) after `go mod download`.

## Evernote

BigBadRescue is an independent tool. It is not made, endorsed or supported by
Evernote. "Evernote" is a trademark of its owner and is used here only to say
which service the tool works with. BigBadRescue talks to Evernote only through
Evernote's public MCP server, signed in with your own account.
