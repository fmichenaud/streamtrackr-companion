;------------------------------------------------------------------------------
; StreamTrackr Companion — Windows installer
;
; Run from companion-app/ via:
;   make installer       (Makefile target — builds .exe first)
;   makensis installer/installer.nsi  (direct, if binary is already built)
;
; Output: installer/streamtrackr-companion-setup.exe
;
; Code-signing flow (CI only — local builds skip this):
;   1. makensis -DPRESIGN  → builds a stub installer whose only job is to
;      drop an unsigned uninstall.exe (containing the FULL uninstall logic,
;      compiled from the shared Section "Uninstall" below) next to itself,
;      then exits silently.
;   2. CI signs that uninstall.exe via Microsoft Trusted Signing.
;   3. makensis -DSIGNED_UNINSTALLER=path → builds the real installer,
;      embedding the now-signed uninstall.exe via File (instead of the
;      usual WriteUninstaller, which would emit an unsigned binary at
;      install time and trigger its own SmartScreen prompt on uninstall).
;   See .github/workflows/release.yml for the orchestration.
;
;   Key invariant: Section "Uninstall" lives at the bottom OUTSIDE the
;   !ifdef branches, so both passes see the exact same uninstall code.
;   This guarantees the signed uninst.exe matches the real installer.
;
; The installer:
;   • Drops streamtrackr-companion.exe into %LocalAppData%\Programs
;   • Creates a Start Menu shortcut group (and optional desktop shortcut)
;   • Adds an HKCU\…\Run entry for "auto-start at boot" (opt-in checkbox,
;     CHECKED by default per the product spec)
;   • Registers itself with Windows so it appears in Settings → Apps,
;     surfacing the standard "Uninstall" affordance.
;
; No Steamworks SDK is shipped. The companion reads achievement state
; directly from Steam's own appcache files (<Steam>\appcache\stats\) —
; no DLL, no SDK init, no anti-cheat surface. See stats_reader.go.
;
; HKCU (not HKLM) for Run + uninstaller: avoids elevation when toggling
; auto-start at runtime from the tray. The installer itself runs admin
; because Program Files is system-protected.
;------------------------------------------------------------------------------

!include "MUI2.nsh"
!include "LogicLib.nsh"

;----- Product identity -------------------------------------------------------
!define APP_NAME      "StreamTrackr Companion"
!define APP_SHORTNAME "StreamTrackrCompanion"
!define APP_EXE       "streamtrackr-companion.exe"
!define APP_PUBLISHER "StreamTrackr"
!define APP_URL       "https://streamtrackr.com"
; APP_VERSION defaults to a dev string; the Makefile overrides via
;   makensis -DAPP_VERSION=$(VERSION) installer.nsi
; so packaged installers always carry the same version as the embedded
; .exe (via -ldflags "-X main.version=…").
!ifndef APP_VERSION
  !define APP_VERSION "dev"
!endif

; Where to write the uninstall metadata Windows reads for Add/Remove Programs.
; HKCU (per-user) so the install never asks for admin, and the same user
; can later uninstall without elevation either.
!define UNINSTALL_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APP_SHORTNAME}"
; The HKCU run-key entry the tray toggles. MUST match the constants in
; autostart_windows.go — both sides read/write the same value name.
!define AUTOSTART_KEY   "Software\Microsoft\Windows\CurrentVersion\Run"
!define AUTOSTART_VALUE "StreamTrackrCompanion"

;----- Macro: close any running companion instance ---------------------------
; Windows holds a file lock on the executable while the tray app is running,
; so File / Delete on streamtrackr-companion.exe fails with an error mid-flow.
; Both the installer (manual upgrade scenario) and the uninstaller need to
; close the app before touching the .exe.
;
; Strategy: try a graceful close first (taskkill without /F sends WM_CLOSE
; to GUI windows, giving the tray app a chance to flush its log and unwind
; the watcher goroutine), wait a beat, then force-kill any survivors. The
; companion has no in-memory state that survives a hard kill — the watcher
; is restarted from scratch on next launch.
;
; SetDetailsPrint silences the "killed process X" line in the install log
; (the tail end of an uninstall is already noisy enough).

!macro CloseRunningCompanion
  DetailPrint "Closing any running ${APP_NAME}…"
  SetDetailsPrint none
  ; First pass — graceful. Ignore errors: 128 = "no such process", which
  ; just means the app wasn't running. Any other error is non-fatal too.
  nsExec::Exec 'taskkill /IM "${APP_EXE}"'
  Pop $0
  Sleep 800
  ; Second pass — force. Catches stuck instances or apps that ignored
  ; the WM_CLOSE (e.g. a wedged systray callback).
  nsExec::Exec 'taskkill /F /IM "${APP_EXE}"'
  Pop $0
  Sleep 300
  SetDetailsPrint both
!macroend

;----- Installer metadata -----------------------------------------------------
Name "${APP_NAME}"
OutFile "streamtrackr-companion-setup.exe"
; Per-user install in %LocalAppData%\Programs — matches VS Code, Discord,
; Slack, Spotify. No UAC at install time AND no UAC for the auto-updater
; later (the running .exe sits in a directory the current user owns, so
; go-update's atomic-rename trick works without elevation).
InstallDir "$LOCALAPPDATA\Programs\${APP_NAME}"
InstallDirRegKey HKCU "Software\${APP_SHORTNAME}" "InstallDir"
RequestExecutionLevel user
SetCompressor /SOLID lzma   ; smaller installer, 1-shot decompression
Unicode true

;----- PRESIGN stub vs real installer pages -----------------------------------
; The two passes diverge here: PRESIGN doesn't show any installer UI (we
; just need uninstall.exe written to disk). The real installer goes through
; the full MUI wizard.
!ifdef PRESIGN
  SilentInstall silent
  ShowInstDetails hide
!else
  ;----- Visual identity ------------------------------------------------------
  !define MUI_ICON   "..\assets\icon.ico"
  !define MUI_UNICON "..\assets\icon.ico"
  !define MUI_ABORTWARNING

  ;----- Install wizard pages -------------------------------------------------
  !insertmacro MUI_PAGE_WELCOME
  !insertmacro MUI_PAGE_DIRECTORY
  !insertmacro MUI_PAGE_COMPONENTS
  !insertmacro MUI_PAGE_INSTFILES

  ; Finish page — give the user the option to launch the tray immediately.
  !define MUI_FINISHPAGE_RUN "$INSTDIR\${APP_EXE}"
  !define MUI_FINISHPAGE_RUN_TEXT "Launch ${APP_NAME} now"
  !insertmacro MUI_PAGE_FINISH
!endif

;----- Uninstaller wizard pages (always present) ------------------------------
; Both passes need these: the PRESIGN stub compiles them into uninstall.exe
; so the signed uninstaller has a UI when the user runs it. In the real
; installer pass they're technically wasted (we embed the pre-signed
; uninst.exe rather than generating one) but they don't break anything.
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "English"

;----- Section descriptions (shown in the components page) --------------------
; Only used by the real installer's components page; PRESIGN doesn't show
; one. Defined unconditionally so the LangString references in the
; description macros below are always valid.
LangString DESC_SecCore         ${LANG_ENGLISH} "Main application (required)."
LangString DESC_SecAutostart    ${LANG_ENGLISH} "Launch the companion automatically when Windows starts."
LangString DESC_SecDesktop      ${LANG_ENGLISH} "Create a Desktop shortcut."

;----- INSTALL sections -------------------------------------------------------
!ifdef PRESIGN
  ; Stub: emit uninstall.exe next to the running installer, then exit.
  ; $EXEDIR is the directory containing the installer — in CI, that's
  ; installer/, which is also where the workflow expects to find the
  ; resulting uninstall.exe for the subsequent sign step.
  Section
    WriteUninstaller "$EXEDIR\uninstall.exe"
  SectionEnd
!else
  Section "${APP_NAME}" SecCore
    SectionIn RO            ; required, can't be unchecked
    SetOutPath "$INSTDIR"

    ; Close any running companion before overwriting its .exe. Covers
    ; the "manual re-run of installer to upgrade" path — the in-app
    ; auto-updater uses go-update which doesn't need this.
    !insertmacro CloseRunningCompanion

    ; Core files — single .exe, no native DLLs to bundle. Achievement
    ; data is read straight from Steam's appcache (see stats_reader.go).
    File "..\${APP_EXE}"
    File "..\README.md"

    ; Start Menu shortcuts.
    CreateDirectory "$SMPROGRAMS\${APP_NAME}"
    CreateShortcut  "$SMPROGRAMS\${APP_NAME}\${APP_NAME}.lnk" "$INSTDIR\${APP_EXE}" "" "$INSTDIR\${APP_EXE}" 0
    CreateShortcut  "$SMPROGRAMS\${APP_NAME}\Uninstall.lnk" "$INSTDIR\uninstall.exe"

    ; Register with Windows so we appear in Settings → Apps & Features
    ; (lets the user uninstall via the standard OS UI, not just the Start
    ; Menu). HKCU because the per-user install lives in %LocalAppData%.
    WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayName"     "${APP_NAME}"
    WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayVersion"  "${APP_VERSION}"
    WriteRegStr HKCU "${UNINSTALL_KEY}" "Publisher"       "${APP_PUBLISHER}"
    WriteRegStr HKCU "${UNINSTALL_KEY}" "URLInfoAbout"    "${APP_URL}"
    WriteRegStr HKCU "${UNINSTALL_KEY}" "DisplayIcon"     "$INSTDIR\${APP_EXE},0"
    WriteRegStr HKCU "${UNINSTALL_KEY}" "InstallLocation" "$INSTDIR"
    WriteRegStr HKCU "${UNINSTALL_KEY}" "UninstallString" "$INSTDIR\uninstall.exe"
    WriteRegDWORD HKCU "${UNINSTALL_KEY}" "NoModify" 1
    WriteRegDWORD HKCU "${UNINSTALL_KEY}" "NoRepair" 1

    WriteRegStr HKCU "Software\${APP_SHORTNAME}" "InstallDir" "$INSTDIR"

    ; Uninstaller — see the file header for the rationale on the two paths.
    ; SIGNED_UNINSTALLER is set by the CI release workflow; local dev /
    ; unsigned makensis runs fall through to WriteUninstaller.
    !ifdef SIGNED_UNINSTALLER
      File "/oname=uninstall.exe" "${SIGNED_UNINSTALLER}"
    !else
      WriteUninstaller "$INSTDIR\uninstall.exe"
    !endif
  SectionEnd

  Section "Launch at Windows startup" SecAutostart
    ; HKCU (per-user) — toggling at runtime from the tray doesn't need elevation.
    ; The quoted path matters: Windows shells split on space otherwise.
    WriteRegStr HKCU "${AUTOSTART_KEY}" "${AUTOSTART_VALUE}" '"$INSTDIR\${APP_EXE}"'
  SectionEnd

  Section /o "Desktop shortcut" SecDesktop
    ; /o = unchecked by default. Most streamers already pin from Start Menu;
    ; we don't want to clutter the desktop without consent.
    CreateShortcut "$DESKTOP\${APP_NAME}.lnk" "$INSTDIR\${APP_EXE}" "" "$INSTDIR\${APP_EXE}" 0
  SectionEnd

  !insertmacro MUI_FUNCTION_DESCRIPTION_BEGIN
    !insertmacro MUI_DESCRIPTION_TEXT ${SecCore}      $(DESC_SecCore)
    !insertmacro MUI_DESCRIPTION_TEXT ${SecAutostart} $(DESC_SecAutostart)
    !insertmacro MUI_DESCRIPTION_TEXT ${SecDesktop}   $(DESC_SecDesktop)
  !insertmacro MUI_FUNCTION_DESCRIPTION_END
!endif

;----- UNINSTALL (shared between PRESIGN and real installer) ------------------
; Lives outside the !ifdef so the PRESIGN stub compiles it into uninstall.exe
; (which then gets signed and embedded into the real installer in pass 2).
; The real installer pass 2 also compiles this code but discards it — NSIS
; warns about that ("Uninstaller script code found but WriteUninstaller never
; used"), which is expected and harmless.

Section "Uninstall"
  ; Close the tray app first — Windows holds a file lock on the running
  ; .exe and Delete would otherwise fail mid-uninstall, leaving stale
  ; files + registry entries behind.
  !insertmacro CloseRunningCompanion

  ; Files
  Delete "$INSTDIR\${APP_EXE}"
  Delete "$INSTDIR\README.md"
  Delete "$INSTDIR\uninstall.exe"
  ; Older installs (<= v0.x) shipped a bundled steam_api64.dll — remove
  ; it on uninstall if present so the install dir cleans up to empty.
  ; Silently ignored when the file doesn't exist.
  Delete "$INSTDIR\steam_api64.dll"
  RMDir  "$INSTDIR"

  ; Shortcuts
  Delete "$SMPROGRAMS\${APP_NAME}\${APP_NAME}.lnk"
  Delete "$SMPROGRAMS\${APP_NAME}\Uninstall.lnk"
  RMDir  "$SMPROGRAMS\${APP_NAME}"
  Delete "$DESKTOP\${APP_NAME}.lnk"

  ; Auto-start entry (HKCU — may not exist; DeleteRegValue silently ignores).
  DeleteRegValue HKCU "${AUTOSTART_KEY}" "${AUTOSTART_VALUE}"

  ; Uninstaller / install root keys (HKCU per the per-user install model).
  DeleteRegKey HKCU "${UNINSTALL_KEY}"
  DeleteRegKey HKCU "Software\${APP_SHORTNAME}"

  ; NOTE — we intentionally don't touch:
  ;   • %AppData%\StreamTrackr\token.json   (user can re-uninstall + re-install
  ;     without re-pairing — destructive cleanup is the wrong default)
  ;   • %AppData%\StreamTrackr\companion.log
  ; The user can wipe these by hand if they want a clean slate.
SectionEnd
