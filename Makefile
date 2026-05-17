#
# StreamTrackr Companion build pipeline.
#
# Targets:
#   make build       Cross-compile the Windows .exe (tray + cgo).
#   make installer   Build the NSIS installer (depends on `build`).
#   make mac         Sanity build on the host (no .exe, no tray UI).
#   make clean       Remove built artefacts.
#
# Requirements (one-time setup):
#   brew install mingw-w64 makensis
#

BINARY      := streamtrackr-companion.exe
INSTALLER   := installer/streamtrackr-companion-setup.exe
# Embedded Windows resources (icon). `go build` picks up the .syso
# automatically thanks to the `_windows_amd64.syso` suffix.
SYSO        := rsrc_windows_amd64.syso
RC          := companion.rc

# Version stamped into the binary via -ldflags. The auto-updater compares
# this to the manifest's `version` field; a dev build (no override) keeps
# the default "dev" sentinel and is excluded from update polling.
#
# Override at the CLI:
#   make installer VERSION=0.2.0
VERSION     ?= dev

# Cross-compile environment. CGO is required by github.com/getlantern/systray.
# -H=windowsgui suppresses the auto-allocated console window; ensureConsole()
# re-attaches one when the user invokes a CLI sub-command from cmd.exe.
GO_ENV      := CGO_ENABLED=1 \
               CC=x86_64-w64-mingw32-gcc \
               CXX=x86_64-w64-mingw32-g++ \
               GOOS=windows GOARCH=amd64
GO_LDFLAGS  := -s -w -H=windowsgui -X main.version=$(VERSION)

.PHONY: build installer mac clean help

build: $(BINARY)
	@ls -lh $(BINARY)

$(BINARY): $(wildcard *.go) $(SYSO) go.mod go.sum
	@echo "→ cross-compiling Windows binary…"
	$(GO_ENV) go build -ldflags="$(GO_LDFLAGS)" -o $@ .

# windres (mingw-w64) compiles the .rc to a COFF object that Go's linker
# attaches under the resource section of the PE. After this, Windows
# Explorer + Start Menu shortcuts pick up the icon automatically.
$(SYSO): $(RC) assets/icon.ico
	@echo "→ embedding tray icon into binary resources…"
	x86_64-w64-mingw32-windres -i $(RC) -O coff -o $@

installer: build $(INSTALLER)

# NSIS reads installer/installer.nsi which `File`s in the .exe from the
# parent directory. Output lands next to the .nsi script so the whole
# installer/ folder can be uploaded as one bundle.
$(INSTALLER): installer/installer.nsi $(BINARY)
	@echo "→ building NSIS installer (v$(VERSION))…"
	makensis -V2 -DAPP_VERSION=$(VERSION) installer/installer.nsi
	@ls -lh $(INSTALLER)

mac:
	@echo "→ Mac dev build (no tray on this platform — `help` only)…"
	go build -o /tmp/streamtrackr-companion-mac .
	/tmp/streamtrackr-companion-mac help

clean:
	rm -f $(BINARY) $(INSTALLER) $(SYSO)

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sed -e 's/:.*##/ — /'
