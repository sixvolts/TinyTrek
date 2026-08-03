#!/bin/sh
# One screen of "what is the bench actually running right now".
#
# The failure this exists to prevent: push-dashboard.sh reloads the Go binary in
# two seconds, so the DUT's USERSPACE tracks the source tree continuously while
# its PLATFORM -- bus bitrates, FD mode, gateway units, kernel -- stays frozen at
# whatever .wic was last flashed. Test results then reflect a machine that exists
# nowhere in git. This prints both halves side by side and names the drift.

DUT="${DUT:-192.168.4.133}"
DUT_USER="${DUT_USER:-ttos}"
DUT_PASS="${DUT_PASS:-ttos}"
DRIVE_IF="${TTOS_BENCH_DRIVE_IF:-ttdrive}"
DIAG_IF="${TTOS_BENCH_DIAG_IF:-ttdiag}"
REPO="$(cd "$(dirname "$0")/.." && pwd)"
SSH="ssh -o BatchMode=yes -o ConnectTimeout=6"

RED='\033[31m'; GRN='\033[32m'; YLW='\033[33m'; BLD='\033[1m'; RST='\033[0m'
warn() { printf "${YLW}  !! %s${RST}\n" "$*"; }
bad()  { printf "${RED}  XX %s${RST}\n" "$*"; }

printf "\n${BLD}== bench host (arcana) ==${RST}\n"
for pair in "$DRIVE_IF DRIVE" "$DIAG_IF DIAG"; do
    set -- $pair; i=$1; role=$2
    if ! ip link show "$i" >/dev/null 2>&1; then
        bad "$i ($role) missing -- udev rules not installed?"; continue
    fi
    state=$(ip -br link show "$i" | awk '{print $2}')
    br=$(ip -d link show "$i" | grep -oE 'bitrate [0-9]+' | head -1 | awk '{print $2}')
    dbr=$(ip -d link show "$i" | grep -oE 'dbitrate [0-9]+' | head -1 | awk '{print $2}')
    est=$(ip -d link show "$i" | grep -oE 'can state [A-Z-]+' | awk '{print $3}')
    printf "  %-8s %-6s %-6s arb=%-7s data=%-8s %s\n" \
        "$i" "$role" "$state" "${br:-?}" "${dbr:-classic}" "${est:-}"
    case "$est" in
        ERROR-PASSIVE|BUS-OFF) bad "$i is $est -- termination, wiring, or an FD frame on a classic bus";;
    esac
done

printf "\n${BLD}== DUT ($DUT) ==${RST}\n"
if ! $SSH "$DUT_USER@$DUT" true 2>/dev/null; then
    bad "unreachable over ssh (key auth). Is it powered? On the LAN?"
    printf "\n"; exit 1
fi

$SSH "$DUT_USER@$DUT" "
PATH=/sbin:/usr/sbin:\$PATH
S() { echo '$DUT_PASS' | sudo -S -p '' \"\$@\"; }
echo \"  host      \$(hostname)\"
echo \"  uptime    \$(uptime | sed 's/^ *//')\"
if [ -f /etc/ttos/provision.src ]; then echo '  identity  PROVISIONED'
else echo '  identity  unprovisioned (factory mode: ttos/ttos, open AP)'; fi
echo \"  dashboard \$(systemctl is-active ttos-dashboard)\"
echo \"  cangw     \$(systemctl is-active ttos-cangw)\"
echo '  --- gateway rules (DRIVE->DIAG only; nothing inbound) ---'
S /usr/bin/cangw -L 2>/dev/null | sed 's/^/    /' || true
echo '  --- drive bus (can1) ---'
grep -E '^(BitRate|DataBitRate|FDMode)' /etc/systemd/network/can1.network 2>/dev/null | sed 's/^/    /'
" 2>/dev/null

# ------------------------------------------------------------------ drift -----
printf "\n${BLD}== source vs DUT ==${RST}\n"
printf "  repo HEAD %s\n" "$(cd "$REPO" && git log --oneline -1 2>/dev/null)"

src_fd=$(grep -cE '^FDMode=yes' "$REPO/meta-tt-ctf/recipes-core/systemd/systemd/can1.network" 2>/dev/null)
dut_fd=$($SSH "$DUT_USER@$DUT" "grep -cE '^FDMode=yes' /etc/systemd/network/can1.network 2>/dev/null" 2>/dev/null)
# grep -c prints 0 and exits 1 on no match, so `|| echo 0` would append a SECOND
# zero and the comparison would run on "0\n0". Normalise instead of defaulting.
src_fd=$(printf '%s' "${src_fd:-0}" | tr -dc '0-9' | head -c1)
dut_fd=$(printf '%s' "${dut_fd:-0}" | tr -dc '0-9' | head -c1)

if [ "$src_fd" != "$dut_fd" ]; then
    warn "DRIVE-BUS FD MODE DIFFERS:  source FDMode=yes:$src_fd   DUT FDMode=yes:$dut_fd"
    if [ "$dut_fd" != "0" ]; then
        warn "The DUT can still transmit CAN FD on the drive bus. Real MCP2515 motor"
        warn "nodes read that as a form error -> error-passive -> bus-off -> no drive."
        warn "Matrix G3 cannot be closed on this DUT. Reflash before trusting it."
    fi
    warn "This is a .network file -- push-dashboard.sh CANNOT fix it. Needs a full image."
else
    printf "${GRN}  drive-bus FD mode matches source${RST}\n"
fi
printf "\n"
