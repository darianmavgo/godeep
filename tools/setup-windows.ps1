<#
.SYNOPSIS
  Minimal Windows 11 setup to push a godeep kernel to the MacBook over USB-C.

.DESCRIPTION
  The PC does not run godeep (no QEMU). It only talks to m1n1's proxy on the
  Mac and loads bin/godeep-applem1.img into it (docs/Using2Computers.md, loop 3a).

  Windows: WSL2 + Ubuntu, usbipd-win (forwards m1n1's USB device into WSL).
  Ubuntu:  python3 + m1n1's proxyclient (~/m1n1, ~/m1n1-venv), M1N1DEVICE,
           dialout group for /dev/ttyACM0, and a `godeep-push` command.
  Unless -NoBuild: host Go + TamaGo Go + a clone of godeep in ~/godeep, so
           godeep-push can build the Mac image first. With -NoBuild, copy
           godeep-applem1.img from the Mac into ~/godeep/bin yourself.

  Either way you also need the Mac's device tree as ~/godeep/bin/t6000-j316s.dtb
  (copy it from the Mac).

  Safe to re-run. If it asks for a reboot or an Ubuntu user, do that and re-run.

    powershell -ExecutionPolicy Bypass -File tools\setup-windows.ps1 [-NoBuild]
#>
[CmdletBinding()]
param(
    [string]$Distro    = "Ubuntu",
    [switch]$NoBuild,
    # Only used without -NoBuild. Keep TamagoTag matching github.com/usbarmory/tamago in go.mod.
    [string]$GoVersion = "1.27.4",
    [string]$TamagoTag = "tamago-go1.27.4",
    [string]$RepoUrl   = "https://github.com/darianmavgo/godeep.git"
)

$ErrorActionPreference = "Stop"
$env:WSL_UTF8 = "1"   # wsl.exe prints UTF-16 otherwise

function Step($msg) { Write-Host "`n==> $msg" -ForegroundColor Cyan }
function Ok($msg)   { Write-Host "    $msg" -ForegroundColor Green }
function Warn($msg) { Write-Host "    $msg" -ForegroundColor Yellow }

# --- Elevate -----------------------------------------------------------------
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).
    IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin) {
    $argList = @("-NoProfile", "-ExecutionPolicy", "Bypass", "-NoExit", "-File", "`"$PSCommandPath`"")
    foreach ($kv in $PSBoundParameters.GetEnumerator()) {
        if ($kv.Value -is [switch]) { if ($kv.Value) { $argList += "-$($kv.Key)" } }
        else { $argList += "-$($kv.Key)", "`"$($kv.Value)`"" }
    }
    Start-Process powershell -Verb RunAs -ArgumentList $argList
    exit
}

# --- 1. WSL2 + Ubuntu --------------------------------------------------------
Step "WSL2 + $Distro"
$null = wsl.exe --status 2>&1
if ($LASTEXITCODE -ne 0) {
    wsl.exe --install --no-distribution
    Warn "WSL was just installed. Reboot Windows, then run this script again."
    exit 0
}
function DistroList { (wsl.exe -l -q) -replace "`0", "" | ForEach-Object { $_.Trim() } | Where-Object { $_ } }
if ((DistroList) -notcontains $Distro) {
    Write-Host "    Installing $Distro. Create a Linux user when asked; type 'exit' at the Linux prompt to continue."
    wsl.exe --install -d $Distro
    if ((DistroList) -notcontains $Distro) {
        Warn "Reboot, open '$Distro' from the Start menu once to create your user, then run this again."
        exit 0
    }
}
$wslUser = (wsl.exe -d $Distro -- id -un).Trim()
if ($wslUser -eq "root") {
    Warn "Open '$Distro' from the Start menu, create your user, then run this again."
    exit 0
}
$goArch = @{ "x86_64" = "amd64"; "aarch64" = "arm64" }[(wsl.exe -d $Distro -- uname -m).Trim()]
Ok "$Distro, user $wslUser"

# --- 2. usbipd-win -----------------------------------------------------------
Step "usbipd-win"
$null = winget list --id dorssel.usbipd-win --exact --accept-source-agreements 2>$null
if ($LASTEXITCODE -ne 0) {
    winget install --id dorssel.usbipd-win --exact --silent --accept-package-agreements --accept-source-agreements
    if ($LASTEXITCODE -ne 0) { throw "winget install usbipd-win failed" }
}
$env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")
Ok "installed"

function Invoke-WslScript([string]$script, [string]$user, [string[]]$scriptArgs) {
    $tmp = Join-Path $env:TEMP ("godeep-setup-" + [guid]::NewGuid().ToString("N") + ".sh")
    [IO.File]::WriteAllText($tmp, ($script -replace "`r`n", "`n"), (New-Object Text.UTF8Encoding $false))
    try {
        $wslPath = (wsl.exe -d $Distro -- wslpath -a ($tmp -replace '\\', '/')).Trim()
        wsl.exe -d $Distro -u $user -- bash $wslPath @scriptArgs
        if ($LASTEXITCODE -ne 0) { throw "WSL script failed as $user (exit $LASTEXITCODE)" }
    } finally { Remove-Item $tmp -ErrorAction SilentlyContinue }
}
$build = if ($NoBuild) { "0" } else { "1" }

# --- 3. Ubuntu packages (root) -----------------------------------------------
Step "Ubuntu packages"
$rootScript = @'
set -euo pipefail
BUILD="$1"; GOVER="$2"; GOARCH="$3"; USERNAME="$4"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q git curl ca-certificates python3 python3-venv python3-pip
usermod -aG dialout "$USERNAME"     # /dev/ttyACM0
if [ "$BUILD" = 1 ]; then
    apt-get install -y -q build-essential make
    if [ "$(/usr/local/go/bin/go env GOVERSION 2>/dev/null || true)" != "go$GOVER" ]; then
        tmp=$(mktemp -d)
        curl -fsSL "https://go.dev/dl/go$GOVER.linux-$GOARCH.tar.gz" -o "$tmp/go.tgz"
        rm -rf /usr/local/go && tar -C /usr/local -xzf "$tmp/go.tgz" && rm -rf "$tmp"
    fi
fi
'@
Invoke-WslScript $rootScript "root" @($build, $GoVersion, $goArch, $wslUser)
Ok "done"

# --- 4. m1n1 proxyclient, toolchain, godeep-push (user) ----------------------
Step "m1n1 proxyclient$(if (-not $NoBuild) { ', Go + TamaGo, godeep clone' }), godeep-push"
$userScript = @'
set -euo pipefail
BUILD="$1"; TAMAGO_TAG="$2"; REPO="$3"

addrc() { grep -qxF "$1" ~/.bashrc || echo "$1" >> ~/.bashrc; }
addrc 'export M1N1DEVICE=/dev/ttyACM0'
addrc 'export PATH=$HOME/.local/bin:/usr/local/go/bin:$PATH'

# m1n1's proxyclient: talks to the Mac's m1n1 over USB
if [ ! -d ~/m1n1/.git ]; then
    git clone --depth 1 --recursive https://github.com/AsahiLinux/m1n1 ~/m1n1
fi
[ -d ~/m1n1-venv ] || python3 -m venv ~/m1n1-venv
~/m1n1-venv/bin/pip install -q pyserial construct
for req in ~/m1n1/proxyclient/requirements.txt ~/m1n1/requirements.txt; do
    if [ -f "$req" ]; then ~/m1n1-venv/bin/pip install -q -r "$req"; fi
done

mkdir -p ~/godeep/bin
if [ "$BUILD" = 1 ]; then
    export PATH=/usr/local/go/bin:$PATH
    if [ ! -x ~/tamago-go/bin/go ] || [ "$(cat ~/tamago-go/.godeep-tag 2>/dev/null)" != "$TAMAGO_TAG" ]; then
        rm -rf ~/tamago-go
        git clone --depth 1 --branch "$TAMAGO_TAG" https://github.com/usbarmory/tamago-go ~/tamago-go
        (cd ~/tamago-go/src && ./make.bash)
        echo "$TAMAGO_TAG" > ~/tamago-go/.godeep-tag
    fi
    if [ ! -d ~/godeep/.git ]; then
        if [ -z "$(ls -A ~/godeep/bin)" ]; then rmdir ~/godeep/bin ~/godeep; fi
        git clone "$REPO" ~/godeep || echo "WARNING: couldn't clone $REPO (private? 'gh auth login' or an SSH key, then clone to ~/godeep)" >&2
        mkdir -p ~/godeep/bin
    fi
fi

# godeep-push: (build and) load the Mac image into m1n1's proxy, then jump to it
mkdir -p ~/.local/bin
cat > ~/.local/bin/godeep-push <<'EOF'
#!/bin/bash
# Usage: godeep-push [--no-build]
# Builds bin/godeep-applem1.img (when ~/godeep is a git clone) and boots it on
# the Mac through m1n1's proxy. Needs bin/t6000-j316s.dtb (copied from the Mac).
set -euo pipefail
cd ~/godeep
export PATH=/usr/local/go/bin:$PATH M1N1DEVICE=${M1N1DEVICE:-/dev/ttyACM0}
if [ "${1:-}" != --no-build ] && [ -d .git ]; then
    git pull --ff-only || true
    make image BOARD=applem1
fi
for f in bin/godeep-applem1.img bin/t6000-j316s.dtb; do
    [ -f "$f" ] || { echo "missing ~/godeep/$f (copy it from the Mac)" >&2; exit 1; }
done
[ -e "$M1N1DEVICE" ] || { echo "$M1N1DEVICE not found: is the Mac in m1n1 proxy mode and attached with usbipd?" >&2; exit 1; }
exec ~/m1n1-venv/bin/python ~/m1n1/proxyclient/tools/linux.py bin/godeep-applem1.img bin/t6000-j316s.dtb
EOF
chmod +x ~/.local/bin/godeep-push
'@
Invoke-WslScript $userScript $wslUser @($build, $TamagoTag, $RepoUrl)
Ok "done"

# --- 5. Bind m1n1's USB device if it's plugged in ----------------------------
Step "m1n1 USB device (1209:316d)"
$m1n1 = usbipd list 2>$null | Select-String "1209:316d"
if ($m1n1) {
    $busid = ($m1n1[0].Line -split '\s+')[0]
    usbipd bind --busid $busid 2>$null
    Ok "Bound $busid."
} else {
    $busid = "<BUSID>"
    Warn "Not connected now. With the Mac in m1n1 proxy mode: usbipd list, then usbipd bind --busid <BUSID> (once)."
}

Step "Done"
Write-Host @"
    Each session, in an admin PowerShell (leave it open; it re-attaches after every Mac reboot):
      usbipd attach --wsl --busid $busid --auto-attach

    Then in $Distro (a new shell, so the dialout group applies):
      godeep-push            # $(if ($NoBuild) { 'loads ~/godeep/bin/godeep-applem1.img' } else { 'pulls, builds the Mac image, loads it' })

    Copy the Mac's device tree to ~/godeep/bin/t6000-j316s.dtb first.
    Mac side (proxy boot entry as default startup disk): docs/Using2Computers.md section 4.
"@
