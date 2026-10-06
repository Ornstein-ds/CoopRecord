param([switch]$TestAudio)
$ErrorActionPreference = 'Stop'
Push-Location $PSScriptRoot
try {
    $localGo = Join-Path $PSScriptRoot '.tools/go/bin/go.exe'
    $goTool = if (Test-Path -LiteralPath $localGo) { $localGo } else { (Get-Command go -ErrorAction Stop).Source }
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $env:GOCACHE = Join-Path $PSScriptRoot '.tools/gocache'
    $env:GOMODCACHE = Join-Path $PSScriptRoot '.tools/mod'
    if ($TestAudio) { $env:COOPRECORD_AUDIO_SMOKE = '1' }
    & $goTool test ./...
    if ($LASTEXITCODE -ne 0) { throw 'Tests failed' }
    & $goTool vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }
    New-Item -ItemType Directory -Force 'dist' | Out-Null
    & $goTool build -buildvcs=false -trimpath -ldflags '-H=windowsgui -s -w' -o dist/CoopRecord.exe .
    if ($LASTEXITCODE -ne 0) { throw 'Build failed. Close CoopRecord if it is running.' }
    Copy-Item -LiteralPath 'README.md' -Destination 'dist/README.md' -Force
    Copy-Item -LiteralPath 'THIRD_PARTY_NOTICES.txt' -Destination 'dist/THIRD_PARTY_NOTICES.txt' -Force
    Compress-Archive -LiteralPath 'dist/CoopRecord.exe','dist/README.md','dist/THIRD_PARTY_NOTICES.txt' -DestinationPath 'dist/CoopRecord-windows-x64.zip' -Force
    $checksum = (Get-FileHash -LiteralPath 'dist/CoopRecord-windows-x64.zip' -Algorithm SHA256).Hash.ToLowerInvariant()
    Set-Content -LiteralPath 'dist/SHA256SUMS.txt' -Value "$checksum  CoopRecord-windows-x64.zip" -Encoding ascii
    Write-Output "Ready: $PSScriptRoot\dist\CoopRecord.exe"
} finally {
    Pop-Location
}
