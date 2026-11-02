#!/bin/bash
# Shared Developer ID identity selection for build-pkg.sh and build-agent-pkg.sh.
# Sourced, not executed.
#
# Why this exists: a renewed/regenerated Developer ID certificate carries exactly
# the same name as the one it replaces ("Developer ID Application: Name (TEAM)").
# Once both are in the keychain, signing *by name* fails with
#   "...: ambiguous (matches "..." and "..." in login.keychain-db)"
# and the old `grep | head -n 1` picked whichever happened to be listed first.
# Instead, among the VALID identities (certificate + private key present, not
# expired) we pick the one that expires last — ties go to the most recently
# issued — and sign by its SHA-1 hash, which is unambiguous.

# Prints "<notAfter><notBefore>" as zero-padded epoch seconds (so a plain string
# comparison orders by expiry first, then issue date) for the certificate with
# SHA-1 hash $2 whose common name is $1. Returns non-zero if it can't be read.
_cert_sort_key() {
    local dates not_after not_before
    dates="$(security find-certificate -a -Z -p -c "$1" 2>/dev/null \
        | awk -v h="$2" '/^SHA-1 hash:/ { p = ($3 == h) } p' \
        | openssl x509 -noout -startdate -enddate 2>/dev/null)" || return 1
    not_before="$(printf '%s\n' "$dates" | sed -n 's/^notBefore=//p')"
    not_after="$(printf '%s\n' "$dates" | sed -n 's/^notAfter=//p')"
    [ -n "$not_before" ] && [ -n "$not_after" ] || return 1
    printf '%012d%012d' \
        "$(date -j -u -f "%b %d %T %Y %Z" "$not_after" +%s)" \
        "$(date -j -u -f "%b %d %T %Y %Z" "$not_before" +%s)"
}

# pick_developer_id_identity <Application|Installer>
# Sets PICKED_IDENTITY to the chosen certificate's SHA-1 hash (empty if none).
pick_developer_id_identity() {
    local kind="$1" policy="codesigning" hash name key best_key=""
    # Installer certificates only show up under the basic policy.
    [ "$kind" = "Installer" ] && policy="basic"
    PICKED_IDENTITY=""
    while read -r hash name; do
        key="$(_cert_sort_key "$name" "$hash")" || continue
        if [[ "$key" > "$best_key" ]]; then
            best_key="$key"
            PICKED_IDENTITY="$hash"
        fi
    done < <(security find-identity -v -p "$policy" 2>/dev/null \
        | sed -nE "s/^ *[0-9]+\) ([0-9A-F]{40}) \"(Developer ID $kind: .*)\"$/\1 \2/p")
}

# describe_signing_identity <sha1-or-name>
# Human-readable label for log output: "Developer ID Application: Name (TEAM),
# expires 2027-02-01 [E8D26F62]". A plain name (e.g. from APP_SIGN_IDENTITY) is
# returned unchanged.
describe_signing_identity() {
    local id="$1" line name expiry
    if [[ "$id" =~ ^[0-9A-Fa-f]{40}$ ]]; then
        line="$(security find-identity -v 2>/dev/null | grep -i "$id" | head -n 1)"
        name="$(printf '%s\n' "$line" | sed -nE 's/.*"(.*)".*/\1/p')"
        expiry="$(security find-certificate -a -Z -p -c "$name" 2>/dev/null \
            | awk -v h="$(printf '%s' "$id" | tr 'a-f' 'A-F')" '/^SHA-1 hash:/ { p = ($3 == h) } p' \
            | openssl x509 -noout -enddate 2>/dev/null | sed 's/^notAfter=//')"
        if [ -n "$name" ] && [ -n "$expiry" ]; then
            printf '%s, expires %s [%s]' "$name" \
                "$(date -j -u -f "%b %d %T %Y %Z" "$expiry" +%Y-%m-%d)" "$(printf '%s' "$id" | cut -c1-8)"
            return
        fi
    fi
    printf '%s' "$id"
}
