<#
.SYNOPSIS
  Sets up a Windows 11 PC as the godeep dev host, with the MacBook as the
  target (docs/Using2Computers.md, sections 3.1-3.5).

.DESCRIPTION
  On Windows: WSL2 + Ubuntu, usbipd-win (USB passthrough of m1n1's proxy
  device into WSL), and optionally VS Code.
  Inside Ubuntu (as root): build tools, QEMU, e2fsprogs/dosfstools/mtools,
  picocom, the aarch64 cross compiler, host Go in /usr/local/go, and the
  dialout group for /dev/ttyACM*.
  Inside Ubuntu (as your user): TamaGo Go in ~/tamago-go, m1n1 and its
  proxyclient venv in ~/m1n1 and ~/m1n1-venv, M1N1DEVICE, the godeep clone
  in ~/godeep (with the progress-log git hooks), and Claude Code.

  Safe to re-run: each step is skipped when it is already done. If it asks
  for a reboot or for an Ubuntu user to be created, do that and run it again.

  Run from any PowerShell (it elevates itself):
    powershell -ExecutionPolicy Bypass -File tools\setup-windows.ps1
  or, with the build checks at the end:
    powershell -ExecutionPolicy Bypass -File tools\setup-windows.ps1 -Verify

.PARAMETER Verify
  After setup, run `make test`, `make image BOARD=applem1` and
  `make check-applem1` in ~/godeep (several minutes).
#>
[CmdletBinding()]
param(
    [string]$Distro     = "Ubuntu",
    # Host Go and TamaGo Go. Keep TamagoTag matching github.com/usbarmory/tamago in go.mod.
    [string]$GoVersion  = "1.27.4",
    [string]$TamagoTag  = "tamago-go1.27.4",
    [string]$RepoUrl    = "https://github.com/darianmavgo/godeep.git",
    [switch]$SkipVSCode,
    [switch]$SkipClaude,
    [switch]$SkipM1n1Build,
    [switch]$Verify
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
    Write-Host "Restarting as administrator..."
    $argList = @("-NoProfile", "-ExecutionPolicy", "Bypass", "-NoExit", "-File", "`"$PSCommandPath`"")
    foreach ($kv in $PSBoundParameters.GetEnumerator()) {
        if ($kv.Value -is [switch]) { if ($kv.Value) { $argList += "-$($kv.Key)" } }
        else { $argList += "-$($kv.Key)", "`"$($kv.Value)`"" }
    }
    Start-Process powershell -Verb RunAs -ArgumentList $argList
    exit
}

if ([Environment]::OSVersion.Version.Build -lt 22000) {
    Warn "This doesn't look like Windows 11 (build $([Environment]::OSVersion.Version.Build)). Continuing anyway."
}

# --- winget ------------------------------------------------------------------
if (-not (Get-Command winget -ErrorAction SilentlyContinue)) {
    throw "winget not found. Install 'App Installer' from the Microsoft Store, then re-run."
}
function WingetInstall($id, $name) {
    $null = winget list --id $id --exact --accept-source-agreements 2>$null
    if ($LASTEXITCODE -eq 0) { Ok "$name already installed"; return }
    winget install --id $id --exact --silent --accept-package-agreements --accept-source-agreements
    if ($LASTEXITCODE -ne 0) { throw "winget install $id failed ($LASTEXITCODE)" }
    Ok "$name installed"
}

# --- 1. WSL2 -----------------------------------------------------------------
Step "WSL2"
$null = wsl.exe --status 2>&1
if ($LASTEXITCODE -ne 0) {
    wsl.exe --install --no-distribution
    Warn "WSL was just installed. Reboot Windows, then run this script again."
    exit 0
}
wsl.exe --update | Out-Null
wsl.exe --set-default-version 2 | Out-Null
Ok "WSL2 ready"

# --- 2. Ubuntu ---------------------------------------------------------------
Step "$Distro"
function DistroList { (wsl.exe -l -q) -replace "`0", "" | ForEach-Object { $_.Trim() } | Where-Object { $_ } }
if ((DistroList) -notcontains $Distro) {
    Write-Host "    Installing $Distro. It will ask you for a Linux user name and password."
    Write-Host "    When you get a Linux prompt, type 'exit' to come back here."
    wsl.exe --install -d $Distro
    if ((DistroList) -notcontains $Distro) {
        Warn "$Distro isn't registered yet (a reboot may be needed). Reboot, open '$Distro' from the Start menu once to create your user, then run this script again."
        exit 0
    }
}
$wslUser = (wsl.exe -d $Distro -- id -un).Trim()
if ($wslUser -eq "root") {
    Warn "$Distro's default user is root. Open '$Distro' from the Start menu, finish creating your user, then run this script again."
    exit 0
}
$arch = (wsl.exe -d $Distro -- uname -m).Trim()
$goArch = @{ "x86_64" = "amd64"; "aarch64" = "arm64" }[$arch]
if (-not $goArch) { throw "Unsupported WSL architecture: $arch" }
Ok "$Distro, user $wslUser, $arch"

# --- 3. Windows tools ---------------------------------------------------------
Step "usbipd-win (USB passthrough of m1n1's proxy into WSL)"
WingetInstall "dorssel.usbipd-win" "usbipd-win"
$env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")

if (-not $SkipVSCode) {
    Step "VS Code"
    WingetInstall "Microsoft.VisualStudioCode" "VS Code"
    $code = Get-Command code -ErrorAction SilentlyContinue
    if ($code) { & code --install-extension ms-vscode-remote.remote-wsl --force | Out-Null; Ok "WSL extension installed" }
    else { Warn "Open VS Code once and install the 'WSL' extension (ms-vscode-remote.remote-wsl)." }
}

# --- Helper: run a bash script inside WSL ------------------------------------
function Invoke-WslScript([string]$script, [string]$user, [string[]]$scriptArgs) {
    $tmp = Join-Path $env:TEMP ("godeep-setup-" + [guid]::NewGuid().ToString("N") + ".sh")
    [IO.File]::WriteAllText($tmp, ($script -replace "`r`n", "`n"), (New-Object Text.UTF8Encoding $false))
    try {
        $wslPath = (wsl.exe -d $Distro -- wslpath -a ($tmp -replace '\\', '/')).Trim()
        wsl.exe -d $Distro -u $user -- bash $wslPath @scriptArgs
        if ($LASTEXITCODE -ne 0) { throw "WSL setup script failed as $user (exit $LASTEXITCODE)" }
    } finally { Remove-Item $tmp -ErrorAction SilentlyContinue }
}

# --- 4. Ubuntu packages and host Go (root) -----------------------------------
Step "Ubuntu packages, QEMU, host Go $GoVersion"
$rootScript = @'
set -euo pipefail
GOVER="$1"; GOARCH="$2"; USERNAME="$3"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q build-essential git make curl ca-certificates gzip xz-utils \
    python3 python3-pip python3-venv \
    qemu-system-arm qemu-system-x86 qemu-utils \
    e2fsprogs dosfstools mtools openssh-client picocom \
    gcc-aarch64-linux-gnu binutils-aarch64-linux-gnu device-tree-compiler

if [ "$(/usr/local/go/bin/go env GOVERSION 2>/dev/null || true)" != "go$GOVER" ]; then
    tmp=$(mktemp -d)
    curl -fsSL "https://go.dev/dl/go$GOVER.linux-$GOARCH.tar.gz" -o "$tmp/go.tgz"
    rm -rf /usr/local/go && tar -C /usr/local -xzf "$tmp/go.tgz"
    rm -rf "$tmp"
fi
/usr/local/go/bin/go version

# /dev/ttyACM* (m1n1 proxy and the guest's UART)
usermod -aG dialout "$USERNAME"

if qemu-system-aarch64 -machine help | grep -q '^sbsa-ref'; then
    echo "QEMU $(qemu-system-aarch64 --version | head -1 | awk '{print $4}') has sbsa-ref"
else
    echo "WARNING: this QEMU has no sbsa-ref machine; make check-applem1 needs it (newer Ubuntu or QEMU from source)" >&2
fi
'@
Invoke-WslScript $rootScript "root" @($GoVersion, $goArch, $wslUser)
Ok "done"

# --- 5. TamaGo, m1n1, godeep, Claude Code (your user) ------------------------
Step "TamaGo Go, m1n1 proxyclient, godeep clone, Claude Code"
$userScript = @'
set -euo pipefail
TAMAGO_TAG="$1"; REPO="$2"; BUILD_M1N1="$3"; INSTALL_CLAUDE="$4"
export PATH=/usr/local/go/bin:$HOME/go/bin:$HOME/.local/bin:$PATH

addrc() { grep -qxF "$1" ~/.bashrc || echo "$1" >> ~/.bashrc; }
addrc 'export PATH=/usr/local/go/bin:$HOME/go/bin:$HOME/.local/bin:$PATH'
addrc 'export M1N1DEVICE=/dev/ttyACM0'

# TamaGo Go (the Makefile's default TAMAGO is ~/tamago-go/bin/go)
if [ ! -x ~/tamago-go/bin/go ] || [ "$(cat ~/tamago-go/.godeep-tag 2>/dev/null)" != "$TAMAGO_TAG" ]; then
    rm -rf ~/tamago-go
    git clone --depth 1 --branch "$TAMAGO_TAG" https://github.com/usbarmory/tamago-go ~/tamago-go
    (cd ~/tamago-go/src && ./make.bash)
    echo "$TAMAGO_TAG" > ~/tamago-go/.godeep-tag
fi
~/tamago-go/bin/go version

# m1n1 and its proxyclient
if [ ! -d ~/m1n1/.git ]; then
    git clone --recursive https://github.com/AsahiLinux/m1n1 ~/m1n1
else
    (cd ~/m1n1 && git pull --ff-only -q && git submodule update --init --recursive -q) || echo "WARNING: couldn't update ~/m1n1" >&2
fi
[ -d ~/m1n1-venv ] || python3 -m venv ~/m1n1-venv
~/m1n1-venv/bin/pip install -q --upgrade pip
~/m1n1-venv/bin/pip install -q pyserial construct
for req in ~/m1n1/proxyclient/requirements.txt ~/m1n1/requirements.txt; do
    [ -f "$req" ] && ~/m1n1-venv/bin/pip install -q -r "$req"
done
if [ "$BUILD_M1N1" = 1 ]; then
    # m1n1.macho is needed for the hypervisor loop (docs/Using2Computers.md, loop 3b)
    (cd ~/m1n1 && make -j"$(nproc)" ARCH=aarch64-linux-gnu- >/dev/null) \
        && echo "built ~/m1n1/build/m1n1.macho" \
        || echo "WARNING: m1n1 build failed; check m1n1's README for its current build command" >&2
fi

# godeep (inside the Linux filesystem, not /mnt/c: much faster)
if [ ! -d ~/godeep/.git ]; then
    git clone "$REPO" ~/godeep || { echo "WARNING: couldn't clone $REPO (private? set up a GitHub login with 'gh auth login' or an SSH key, then clone to ~/godeep)" >&2; }
fi
if [ -d ~/godeep/.git ]; then
    git -C ~/godeep config core.hooksPath tools/githooks
fi

# Claude Code, inside WSL so it sees Linux paths, make and /dev/ttyACM*
if [ "$INSTALL_CLAUDE" = 1 ] && ! command -v claude >/dev/null; then
    curl -fsSL https://claude.ai/install.sh | bash || echo "WARNING: Claude Code install failed" >&2
fi
'@
Invoke-WslScript $userScript $wslUser @($TamagoTag, $RepoUrl, $(if ($SkipM1n1Build) { "0" } else { "1" }), $(if ($SkipClaude) { "0" } else { "1" }))
Ok "done"

# --- 6. Bind m1n1's USB device if it's plugged in ----------------------------
Step "m1n1 proxy device (USB 1209:316d)"
$m1n1 = usbipd list 2>$null | Select-String "1209:316d"
if ($m1n1) {
    $busid = ($m1n1[0].Line -split '\s+')[0]
    usbipd bind --busid $busid 2>$null
    Ok "Bound $busid. Attach it with (leave that window open):"
    Write-Host "      usbipd attach --wsl --busid $busid --auto-attach"
} else {
    Warn "Not connected now. Once the Mac is in m1n1 proxy mode on a USB-C data cable:"
    Write-Host "      usbipd list                                   # find 1209:316d"
    Write-Host "      usbipd bind --busid <BUSID>                   # once, admin"
    Write-Host "      usbipd attach --wsl --busid <BUSID> --auto-attach"
}

# --- 7. Optional checks ------------------------------------------------------
if ($Verify) {
    Step "make test, make image BOARD=applem1, make check-applem1"
    wsl.exe -d $Distro -u $wslUser -- bash -lc 'cd ~/godeep && export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH && make test && make image BOARD=applem1 && make check-applem1'
    if ($LASTEXITCODE -ne 0) { Warn "A check failed (exit $LASTEXITCODE); see the output above." } else { Ok "all passed" }
}

Step "Done"
Write-Host @"
    Next:
      - Open '$Distro' (or: wsl -d $Distro), then: cd ~/godeep && make test
      - A new shell picks up the dialout group, PATH and M1N1DEVICE.
      - VS Code: run 'code .' from ~/godeep inside WSL.
      - Mac side (proxy boot entry, default startup disk): docs/Using2Computers.md section 4.
"@
