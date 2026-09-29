#!/usr/bin/env sh
# Build the deployable on-chain ledger program.
#
# `--arch v3` is not optional on a current cluster. Agave activates SIMD-0500
# ("Disable deployment of SBPF v0, v1 and v2 programs"), and cargo-build-sbf's
# own default is still v0, so a bare `cargo build-sbf --features onchain`
# produces an artifact the loader refuses at deploy time with
# "Detected sbpf_version required by the executable which are not enabled".
# `solana-test-validator` activates that feature from genesis, so this bites
# locally as well as on devnet.
#
# Verify the version before deploying by reading the ELF's e_flags: the SBPF
# version is the value at byte offset 48, and v3 reads 0x03000000.
set -eu

cargo test --features onchain
cargo build-sbf --features onchain --arch v3

so=target/deploy/slotfare_ledger.so
echo "built $so ($(wc -c < "$so" | tr -d ' ') bytes)"
printf 'e_flags: '
od -An -tx4 -j48 -N4 "$so"
