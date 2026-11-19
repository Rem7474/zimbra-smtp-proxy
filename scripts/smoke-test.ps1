# Test de fumée sur Windows : installation silencieuse, démarrage, dialogue
# SMTP sur 127.0.0.1:1025 et LDAP sur 127.0.0.1:3890, ouverture de la
# fenêtre WebView2, puis désinstallation. Utilisé par la CI.
$ErrorActionPreference = "Stop"
$setup = Join-Path $PSScriptRoot "..\dist\ZimbraSmtpProxy-Setup.exe"
$appDir = Join-Path $env:LOCALAPPDATA "Programs\ZimbraSmtpProxy"
$exe = Join-Path $appDir "zimbra-smtp-proxy.exe"

# Part d'une configuration vierge (valeurs par défaut).
Remove-Item (Join-Path $env:APPDATA "ZimbraSmtpProxy") -Recurse -Force -ErrorAction SilentlyContinue

Write-Host "Installation silencieuse"
Start-Process $setup -ArgumentList "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/TASKS=autostart" -Wait
if (-not (Test-Path $exe)) { throw "exe absent après installation : $exe" }
$run = Get-ItemProperty "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" -Name ZimbraSmtpProxy -ErrorAction SilentlyContinue
if (-not $run) { throw "clé de démarrage automatique absente" }

Write-Host "Démarrage de l'application"
$stderr = Join-Path $env:RUNNER_TEMP "zsp-stderr.txt"
if (-not $env:RUNNER_TEMP) { $stderr = Join-Path $env:TEMP "zsp-stderr.txt" }
$proc = Start-Process $exe -ArgumentList "--settings" -PassThru -RedirectStandardError $stderr
$log = Join-Path $env:APPDATA "ZimbraSmtpProxy\proxy.log"
try {
  $client = $null
  for ($i = 0; $i -lt 30 -and -not $client; $i++) {
    try { $client = New-Object Net.Sockets.TcpClient("127.0.0.1", 1025) } catch { Start-Sleep -Seconds 1 }
  }
  if (-not $client) {
    Write-Host "--- processus terminé : $($proc.HasExited)$(if ($proc.HasExited) { ", code $($proc.ExitCode)" })"
    if (Test-Path $stderr) { Write-Host "--- stderr"; Get-Content $stderr }
    if (Test-Path $log) { Write-Host "--- journal"; Get-Content $log }
    throw "le port 1025 ne s'est pas ouvert"
  }
  $stream = $client.GetStream()
  $reader = New-Object IO.StreamReader($stream)
  $writer = New-Object IO.StreamWriter($stream); $writer.NewLine = "`r`n"; $writer.AutoFlush = $true
  $banner = $reader.ReadLine(); Write-Host "  $banner"
  if (-not $banner.StartsWith("220")) { throw "bannière SMTP inattendue" }
  $writer.WriteLine("EHLO ci")
  $ehlo = @()
  do { $line = $reader.ReadLine(); $ehlo += $line; Write-Host "  $line" } while ($line -match "^250-")
  if (-not ($ehlo -match "AUTH PLAIN")) { throw "AUTH PLAIN non annoncé" }
  $writer.WriteLine("MAIL FROM:<ci@example.org>")
  $resp = $reader.ReadLine(); Write-Host "  $resp"
  if ($resp.StartsWith("250")) { throw "MAIL accepté sans authentification" }
  $writer.WriteLine("QUIT")
  $client.Close()

  Write-Host "Annuaire LDAP (127.0.0.1:3890)"
  $ldapClient = $null
  for ($i = 0; $i -lt 10 -and -not $ldapClient; $i++) {
    try { $ldapClient = New-Object Net.Sockets.TcpClient("127.0.0.1", 3890) } catch { Start-Sleep -Seconds 1 }
  }
  if (-not $ldapClient) { throw "le port LDAP 3890 ne s'est pas ouvert" }
  $ldapClient.ReceiveTimeout = 5000
  $lStream = $ldapClient.GetStream()
  $bindReq = [byte[]](0x30, 0x0c, 0x02, 0x01, 0x01, 0x60, 0x07, 0x02, 0x01, 0x03, 0x04, 0x00, 0x80, 0x00)
  $lStream.Write($bindReq, 0, $bindReq.Length)
  $lStream.Flush()
  $buf = New-Object byte[] 256
  $n = $lStream.Read($buf, 0, $buf.Length)
  if ($n -lt 10 -or $buf[0] -ne 0x30) { throw "réponse LDAP invalide ($n octets)" }
  $hasBindResp = $false
  $hasInvalidCreds = $false
  for ($i = 0; $i -lt $n; $i++) {
    if ($buf[$i] -eq 0x61) { $hasBindResp = $true }
    if ($i + 2 -lt $n -and $buf[$i] -eq 0x0a -and $buf[$i+1] -eq 0x01 -and $buf[$i+2] -eq 0x31) {
      $hasInvalidCreds = $true
    }
  }
  if (-not $hasBindResp -or -not $hasInvalidCreds) {
    throw "BindResponse InvalidCredentials (49) attendu sur le port LDAP 3890"
  }
  $unbindReq = [byte[]](0x30, 0x05, 0x02, 0x01, 0x02, 0x42, 0x00)
  $lStream.Write($unbindReq, 0, $unbindReq.Length)
  $ldapClient.Close()
  Write-Host "  BindResponse 49 (InvalidCredentials) reçu sur 127.0.0.1:3890"

  Write-Host "Fenêtre de configuration (WebView2)"
  $webview = $null
  for ($i = 0; $i -lt 30 -and -not $webview; $i++) {
    Start-Sleep -Seconds 1
    $webview = Get-CimInstance Win32_Process -Filter "Name='msedgewebview2.exe'" | Where-Object ParentProcessId -eq $proc.Id
  }
  if (-not $webview) { throw "aucun processus WebView2 lancé par l'application" }
  for ($i = 0; $i -lt 20 -and -not (Select-String -Path $log -Pattern "fenêtre de configuration chargée" -Quiet); $i++) {
    Start-Sleep -Seconds 1
  }
  if (-not (Select-String -Path $log -Pattern "fenêtre de configuration chargée" -Quiet)) {
    throw "la page n'a pas appelé l'application (pont JavaScript/Go)"
  }
  Write-Host "  page chargée, pont JavaScript/Go opérationnel"
} finally {
  Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
}

if (Test-Path $log) { Write-Host "--- journal"; Get-Content $log }

Write-Host "Désinstallation silencieuse"
Start-Process (Join-Path $appDir "unins000.exe") -ArgumentList "/VERYSILENT", "/SUPPRESSMSGBOXES" -Wait
Start-Sleep -Seconds 2
if (Test-Path $exe) { throw "exe toujours présent après désinstallation" }
if (Get-ItemProperty "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" -Name ZimbraSmtpProxy -ErrorAction SilentlyContinue) {
  throw "clé de démarrage automatique toujours présente"
}
Write-Host "Test de fumée OK"
