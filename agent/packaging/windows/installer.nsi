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

Name "${APP_NAME}"
OutFile "BackupAgentSetup.exe"
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
