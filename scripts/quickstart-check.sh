#!/bin/sh
# QUICKSTART.md's sandbox, run as written: a local chain, a funded sender, a
# stream sent over TCP and verified, and tampered streams refused with the
# statuses the quickstart states. Needs bin/beefcat and bin/devchain
# (make beefcat devchain). Ports 18080 and 9000 on loopback must be free.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
PATH="$root/bin:$PATH"
work=$(mktemp -d)
BEEFCAT_HOME="$work/home"
export BEEFCAT_HOME
chain=http://127.0.0.1:18080

devchain -listen 127.0.0.1:18080 >"$work/devchain.log" 2>&1 &
chainpid=$!
cleanup() {
	kill "$chainpid" 2>/dev/null || true
	if [ -n "$work" ] && [ -d "$work" ]; then
		find "$work" -mindepth 1 -delete
		rmdir "$work"
	fi
}
trap cleanup EXIT

fail() {
	echo "quickstart-check: $*" >&2
	exit 1
}

# The chain answers.
i=0
until beefcat init -devchain "$chain" >"$work/init.out" 2>&1 && beefcat fund -n 8 -coinbase -q >"$work/fund.out" 2>&1; do
	i=$((i + 1))
	[ "$i" -lt 50 ] || { cat "$work/fund.out" >&2; fail "the local chain did not answer"; }
	sleep 0.2
done
key=$(beefcat status | awk '/^identity/{print $2}')
[ -n "$key" ] || fail "no identity"
rx="-network regtest -headers $chain"

# Send over TCP to a listener that pinned the sender.
head -c 5000000 /dev/urandom >"$work/in"
# shellcheck disable=SC2086
beefcat -l 127.0.0.1:9000 $rx -k "$key" >"$work/out" 2>"$work/recv.err" &
rxpid=$!
i=0
until beefcat 127.0.0.1 9000 <"$work/in" 2>"$work/send.err"; do
	i=$((i + 1))
	grep -q "connection refused" "$work/send.err" || { cat "$work/send.err" >&2; fail "send"; }
	[ "$i" -lt 50 ] || fail "the listener did not open its port"
	sleep 0.2
done
wait "$rxpid" || { cat "$work/recv.err" >&2; fail "the listener refused a good stream"; }
cmp -s "$work/in" "$work/out" || fail "the received bytes differ"
grep -q "the far end says: VERIFIED" "$work/send.err" || fail "the sender did not hear the verdict"

# To a file and back.
beefcat -o "$work/s.bcat" <"$work/in" 2>/dev/null
# shellcheck disable=SC2086
beefcat verify $rx -k "$key" "$work/s.bcat" 2>/dev/null | cmp -s - "$work/in" || fail "verify of a file"

# expect STATUS CODE COMMAND...: the command exits STATUS and says CODE.
expect() {
	want=$1
	code=$2
	shift 2
	set +e
	"$@" >"$work/x.out" 2>"$work/x.err"
	got=$?
	set -e
	[ "$got" = "$want" ] || { cat "$work/x.err" >&2; fail "exit $got, want $want: $*"; }
	grep -q "$code" "$work/x.err" || { cat "$work/x.err" >&2; fail "no $code: $*"; }
}

# Cut short: refused, and -save keeps nothing.
head -c 3000000 "$work/s.bcat" >"$work/cut.bcat"
# shellcheck disable=SC2086
expect 3 REFUSED-TRUNCATED beefcat verify $rx -k "$key" -save "$work/kept" "$work/cut.bcat"
[ ! -e "$work/kept" ] || fail "-save kept a refused stream"

# One byte lost in transit.
{ head -c 2000000 "$work/s.bcat"; tail -c +2000002 "$work/s.bcat"; } >"$work/bad.bcat"
# shellcheck disable=SC2086
expect 3 REFUSED- beefcat verify $rx -k "$key" "$work/bad.bcat"

# Another sender pinned.
# shellcheck disable=SC2086
expect 4 REFUSED-UNTRUSTED beefcat verify $rx -k 0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798 "$work/s.bcat"

# No sender pinned: verified, and said loudly.
# shellcheck disable=SC2086
expect 0 "THE SENDER IS NOT PINNED" beefcat verify $rx "$work/s.bcat"

# Retracted: the sweep is mined, and a reader that checks spends refuses.
beefcat sweep -yes >/dev/null 2>&1 || fail "sweep"
# shellcheck disable=SC2086
expect 3 REFUSED-RETRACTED beefcat verify $rx -chain "asset:$chain" -unspent -k "$key" "$work/s.bcat"

echo "quickstart-check: ok"
