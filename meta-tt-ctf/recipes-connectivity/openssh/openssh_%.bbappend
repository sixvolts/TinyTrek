# SSH hardening (§5.5). Root login over SSH stays disabled; serial is the only
# root-capable console path (and even there root is locked -- ops log in as 'ttos'
# and sudo). Password auth is left on for development access by the 'ttos' user;
# provisioning may also drop an authorized_keys.
#
# We delete any pre-existing directive lines before appending ours, because sshd
# honours the FIRST occurrence of a keyword -- a stock "PermitRootLogin yes" earlier
# in the file would otherwise win over an appended "no".
do_install:append() {
    cfg="${D}${sysconfdir}/ssh/sshd_config"
    if [ -f "$cfg" ]; then
        sed -i -E '/^[#[:space:]]*PermitRootLogin([[:space:]]|$)/d' "$cfg"
        sed -i -E '/^[#[:space:]]*PasswordAuthentication([[:space:]]|$)/d' "$cfg"
        sed -i -E '/^[#[:space:]]*PubkeyAuthentication([[:space:]]|$)/d' "$cfg"
        {
            printf '\n# TTOS CTF hardening (§5.5)\n'
            printf 'PermitRootLogin no\n'
            printf 'PasswordAuthentication yes\n'
            printf 'PubkeyAuthentication yes\n'
        } >> "$cfg"
    fi
}
