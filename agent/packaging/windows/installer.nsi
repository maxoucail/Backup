; Backup Agent - installeur Windows (NSIS)
;
; Installation par utilisateur (aucun droit administrateur requis) : le
; binaire est copié dans %LOCALAPPDATA%\BackupAgent et une tâche planifiée
; "au logon" est enregistrée pour l'utilisateur courant. C'est
; volontairement un service *par utilisateur* et non un service Windows
; système : seul un processus tournant dans la session interactive de
; l'utilisateur peut afficher la popup de progression sur son écran.
;
; Compilation : makensis installer.nsi
; (le binaire backup-agent.exe cross-compilé doit être présent à côté de
;  ce script - voir packaging/windows/build.sh)

!define APP_NAME "Backup Agent"
!define APP_EXE "backup-agent.exe"
!define COMPANY "Backup Center"

Name "${APP_NAME}"
OutFile "BackupAgentSetup.exe"
InstallDir "$LOCALAPPDATA\BackupAgent"
RequestExecutionLevel user
SetCompressor /SOLID lzma

Page directory
Page instfiles
UninstPage uninstConfirm
UninstPage instfiles

Section "Install"
	SetOutPath "$INSTDIR"
	File "${APP_EXE}"

	CreateDirectory "$SMPROGRAMS\${APP_NAME}"
	CreateShortcut "$SMPROGRAMS\${APP_NAME}\${APP_NAME}.lnk" "$INSTDIR\${APP_EXE}"
	CreateShortcut "$SMPROGRAMS\${APP_NAME}\Désinstaller.lnk" "$INSTDIR\uninstall.exe"

	WriteUninstaller "$INSTDIR\uninstall.exe"
	WriteRegStr HKCU "Software\${COMPANY}\${APP_NAME}" "InstallDir" "$INSTDIR"
	WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\BackupAgent" "DisplayName" "${APP_NAME}"
	WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\BackupAgent" "UninstallString" "$INSTDIR\uninstall.exe"
	WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\BackupAgent" "Publisher" "${COMPANY}"

	; The agent registers its own logon task and, on first run, opens the
	; local enrollment wizard in the default browser - nothing else to
	; configure here.
	Exec '"$INSTDIR\${APP_EXE}"'
SectionEnd

Section "Uninstall"
	nsExec::Exec 'schtasks /Delete /TN "BackupAgent" /F'
	Delete "$INSTDIR\${APP_EXE}"
	Delete "$INSTDIR\uninstall.exe"
	RMDir "$INSTDIR"
	Delete "$SMPROGRAMS\${APP_NAME}\${APP_NAME}.lnk"
	Delete "$SMPROGRAMS\${APP_NAME}\Désinstaller.lnk"
	RMDir "$SMPROGRAMS\${APP_NAME}"
	DeleteRegKey HKCU "Software\${COMPANY}\${APP_NAME}"
	DeleteRegKey HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\BackupAgent"
SectionEnd
