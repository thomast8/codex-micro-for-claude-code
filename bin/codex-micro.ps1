# Runs codex-micro for this computer. The program is downloaded from the
# GitHub release on first use, and checked against checksums.txt beside this
# script before it is run. A local build in build\ takes precedence.
#
#   codex-micro.ps1 path         print the program's path, fetching it first
#   codex-micro.ps1 <command>    run the program
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

function Fail([string] $message) {
    [Console]::Error.WriteLine("codex-micro: $message")
    exit 1
}

$program = Join-Path (Split-Path -Parent $PSScriptRoot) 'build\codex-micro.exe'
if (-not (Test-Path $program)) {
    $arch = @{ AMD64 = 'amd64'; ARM64 = 'arm64' }[$env:PROCESSOR_ARCHITECTURE]
    if (-not $arch) { Fail "$env:PROCESSOR_ARCHITECTURE is not supported yet" }

    # A line of checksums.txt: "<sha256>  codex-micro-<version>-windows-<arch>.exe"
    $line = Get-Content (Join-Path $PSScriptRoot 'checksums.txt') |
        Where-Object { $_ -match " codex-micro-[0-9.]+-windows-$arch\.exe$" } |
        Select-Object -First 1
    if (-not $line) { Fail "no build for windows/$arch" }
    $sum, $name = $line -split '\s+', 2

    $cache = Join-Path $env:LOCALAPPDATA 'codex-micro\bin'
    $program = Join-Path $cache $name
    if (-not (Test-Path $program)) {
        $version = $name -replace '^codex-micro-([0-9.]+)-.*$', '$1'
        $url = "https://github.com/thomast8/codex-micro-for-claude-code/releases/download/v$version/$name"
        New-Item -ItemType Directory -Force $cache | Out-Null
        $download = Join-Path $cache ".download-$PID"
        try {
            Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $download
            if ((Get-FileHash -Algorithm SHA256 $download).Hash -ne $sum) {
                Fail "$name does not match its checksum; not running it"
            }
            Move-Item -Force $download $program
        } finally {
            Remove-Item -ErrorAction SilentlyContinue $download
        }
    }
}

if ($args.Count -eq 1 -and $args[0] -eq 'path') {
    $program
    exit 0
}
& $program @args
exit $LASTEXITCODE
