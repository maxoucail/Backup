; Backup Agent - installeur Windows (NSIS)
;
; Installation machine (droits administrateur requis) : le binaire est
; copié dans Program Files et enregistré comme un vrai Service Windows
; démarrant automatiquement au démarrage de la machine (avant toute
; connexion utilisateur), avec redémarrage automatique en cas
; d'incident. Seul un administrateur peut l'arrêter ou le désinstaller
; (net stop / services.msc / cet installeur) - un utilisateur standard ne
; le peut pas, ce qui est le mécanisme normal de Windows pour un service,
; pas un artifice de dissimulation.
;
; Le service n'a pas de session graphique propre : pour afficher
; l'assistant de configuration ou la popup de progression, l'agent lance
; un petit processus dans la session de l'utilisateur connecté
; (CreateProcessAsUser, voir internal/winsession).
;
; Compilation : makensis installer.nsi
; (le binaire backup-agent.exe cross-compilé doit être présent à côté de
;  ce script - voir packaging/windows/build.sh)

!define APP_NAME "Backup Agent"
!define APP_EXE "backup-agent.exe"
!define COMPANY "Backup Center"

; APP_VERSION is passed in by build.sh (-DAPP_VERSION=x.y.z); the default
; only applies if makensis is invoked by hand.
!ifndef APP_VERSION
	!define APP_VERSION "0.0.0"
!endif

; The version belongs in the *filename*: a fixed name makes every build
; look identical on the download page, so an operator can't tell whether a
; redeployed agent actually landed, and browsers happily serve a cached
; copy of the old one under the unchanged URL.
Name "${APP_NAME} ${APP_VERSION}"
OutFile "BackupAgentSetup-${APP_VERSION}.exe"

; Same version in the file's own properties, so it's still identifiable
; once downloaded and renamed, and in Programmes et fonctionnalités.
; APP_VERSION4 is the strict x.x.x.x form VIProductVersion demands,
; normalised by build.sh - VIProductVersion rejects anything else outright.
!ifndef APP_VERSION4
	!define APP_VERSION4 "0.0.0.0"
!endif
VIProductVersion "${APP_VERSION4}"
VIAddVersionKey "ProductName" "${APP_NAME}"
VIAddVersionKey "ProductVersion" "${APP_VERSION}"
VIAddVersionKey "FileVersion" "${APP_VERSION}"
VIAddVersionKey "CompanyName" "${COMPANY}"
VIAddVersionKey "FileDescription" "Installeur ${APP_NAME}"
VIAddVersionKey "LegalCopyright" "${COMPANY}"
InstallDir "$PROGRAMFILES64\BackupAgent"
RequestExecutionLevel admin
SetCompressor /SOLID lzma

Page directory
Page instfiles
UninstPage uninstConfirm
UninstPage instfiles

Section "Install"
	SetOutPath "$INSTDIR"
	File "${APP_EXE}"

	CreateDirectory "$SMPROGRAMS\${APP_NAME}"
	CreateShortcut "$SMPROGRAMS\${APP_NAME}\Désinstaller.lnk" "$INSTDIR\uninstall.exe"

	WriteUninstaller "$INSTDIR\uninstall.exe"
	WriteRegStr HKLM "Software\${COMPANY}\${APP_NAME}" "InstallDir" "$INSTDIR"
	WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\BackupAgent" "DisplayName" "${APP_NAME}"
	WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\BackupAgent" "UninstallString" "$INSTDIR\uninstall.exe"
	WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\BackupAgent" "Publisher" "${COMPANY}"
	; Lets an admin confirm which agent version a machine is actually
	; running straight from Programmes et fonctionnalités - the quickest
	; way to spot a poste still on an old build after a fleet update.
	WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\BackupAgent" "DisplayVersion" "${APP_VERSION}"

	; Registers and starts the Windows Service. On first run (device not
	; yet enrolled), the service itself opens the enrollment wizard in the
	; installing user's session - nothing else to do here.
	DetailPrint "Installation du service..."
	nsExec::ExecToLog '"$INSTDIR\${APP_EXE}" install'
SectionEnd

Section "Uninstall"
	DetailPrint "Arrêt et désinstallation du service..."
	nsExec::ExecToLog '"$INSTDIR\${APP_EXE}" uninstall'
	Sleep 1000
	Delete "$INSTDIR\${APP_EXE}"
	Delete "$INSTDIR\uninstall.exe"
	RMDir "$INSTDIR"
	Delete "$SMPROGRAMS\${APP_NAME}\Désinstaller.lnk"
	RMDir "$SMPROGRAMS\${APP_NAME}"
	DeleteRegKey HKLM "Software\${COMPANY}\${APP_NAME}"
	DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\BackupAgent"
SectionEnd
