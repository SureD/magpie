#!/bin/sh
# Pack a finished magpie.app into the zip and dmg that are uploaded, then
# check those copies, not the directory make just wrote.
#
# The bundle must already carry the ad-hoc seal from `make app`. This script
# does not add a Developer ID, a hardened runtime, or a notarization ticket,
# and it does not launch the app. `spctl` is expected to reject the download:
# that rejection is the unnotarized result, and it is not a broken seal.
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
cd "$root"

app="$root/magpie.app"
dist="$root/dist"
zip="$dist/magpie-darwin-arm64-unsigned.zip"
dmg="$dist/magpie-darwin-arm64-unsigned.dmg"
report="$dist/SIGNATURE.txt"

if [ "$(uname -s)" != Darwin ]; then
	echo "pack-app.sh: needs macOS" >&2
	exit 1
fi
if [ "$(uname -m)" != arm64 ]; then
	echo "pack-app.sh: needs an arm64 runner, got $(uname -m)" >&2
	exit 1
fi

work=$(mktemp -d)
mount=
cleanup() {
	if [ -n "$mount" ]; then
		hdiutil detach "$mount" >/dev/null 2>&1 || true
		rm -rf "$mount"
	fi
	rm -rf "$work"
}
trap cleanup EXIT

die() {
	echo "pack-app.sh: $*" >&2
	exit 1
}

# codesign writes its display to stderr. Keep the whole text.
show() {
	codesign -dv --verbose=4 "$1" 2>&1 || true
}

cdhash_of() {
	# Read a saved `codesign -dv` file. A pipe into head would SIGPIPE under set -e.
	awk -F= '/^CDHash=/{print $2; exit}' "$1"
}

# A broken linker seal or a bundle edited after signing. This is the
# "damaged, move it to the Trash" failure, distinct from spctl rejecting
# an intact ad-hoc app that was never notarized.
broken_seal() {
	echo "$1" | grep -E -q \
		'no resources but signature indicates|invalid signature|sealed resource is missing|not signed at all|main executable failed|code has no resources|linker-signed'
}

verify_bundle() {
	label=$1
	bundle=$2
	bin="$bundle/Contents/MacOS/magpie"
	log="$work/$label-verify.txt"
	desc="$work/$label-dv.txt"

	[ -d "$bundle" ] || die "$label: no bundle at $bundle"
	[ -x "$bin" ] || die "$label: Contents/MacOS/magpie is missing or not executable"
	[ -f "$bundle/Contents/_CodeSignature/CodeResources" ] || die "$label: no CodeResources seal"
	[ -f "$bundle/Contents/Info.plist" ] || die "$label: no Info.plist"

	mode=$(stat -f '%OLp' "$bin")
	[ "$mode" = "755" ] || die "$label: executable mode is $mode, want 755"

	file "$bin" > "$work/$label-file.txt"
	grep -q 'Mach-O 64-bit executable arm64' "$work/$label-file.txt" \
		|| die "$label: not a thin arm64 executable: $(cat "$work/$label-file.txt")"
	lipo -info "$bin" > "$work/$label-lipo.txt"
	grep -q 'architecture: arm64' "$work/$label-lipo.txt" \
		|| die "$label: lipo: $(cat "$work/$label-lipo.txt")"

	# Every Mach-O in the bundle, not only the main executable.
	find "$bundle" -type f > "$work/$label-files.txt"
	while read -r f; do
		if file "$f" | grep -q 'Mach-O'; then
			codesign --verify --strict "$f" \
				|| die "$label: nested Mach-O failed codesign --verify --strict: $f"
		fi
	done < "$work/$label-files.txt"

	if ! codesign --verify --deep --strict --verbose=2 "$bundle" >"$log" 2>&1; then
		cat "$log" >&2
		die "$label: codesign --verify --deep --strict failed"
	fi
	grep -q 'valid on disk' "$log" || die "$label: verify did not say valid on disk"
	grep -q 'satisfies its Designated Requirement' "$log" \
		|| die "$label: verify did not satisfy its designated requirement"
	if broken_seal "$(cat "$log")"; then
		cat "$log" >&2
		die "$label: verify output describes a broken seal"
	fi

	show "$bundle" > "$desc"
	fail_desc() {
		cat "$desc" >&2
		die "$label: $*"
	}
	grep -q '^Signature=adhoc$' "$desc" \
		|| fail_desc "signature is not ad-hoc (not a Developer ID, and not unsigned)"
	grep -q '^Identifier=com.yetone.magpie$' "$desc" \
		|| fail_desc "identifier is not com.yetone.magpie (linker left a.out?)"
	grep -q '^TeamIdentifier=not set$' "$desc" \
		|| fail_desc "a team id is present; this build has no certificate"
	grep -q '^Format=app bundle with Mach-O thin (arm64)$' "$desc" \
		|| fail_desc "format line is not a thin arm64 bundle"
	grep -q 'Sealed Resources version=' "$desc" || fail_desc "resources are not sealed"
	grep -q 'Info.plist entries=' "$desc" || fail_desc "Info.plist is not bound into the signature"
	if grep -E -q 'linker-signed|not bound|^Authority=' "$desc"; then
		fail_desc "signature is still the linker seal, unbound, or a certificate chain"
	fi
	hash=$(cdhash_of "$desc")
	[ -n "$hash" ] || die "$label: no CDHash"
	echo "$hash" > "$work/$label-cdhash.txt"
	echo "$label: valid on disk, ad-hoc, arm64, CDHash=$hash"
}

assess() {
	label=$1
	bundle=$2
	out="$work/$label-spctl.txt"
	# Rejection is expected. A broken-seal phrase is not.
	spctl --assess --verbose=4 --type execute "$bundle" >"$out" 2>&1 || true
	echo "---- spctl $label ----"
	cat "$out"
	if broken_seal "$(cat "$out")"; then
		die "$label: spctl reports a broken seal, not an unnotarized ad-hoc app"
	fi
	if grep -E -q 'accepted' "$out" && grep -E -q 'Notarized Developer ID|source=Developer ID' "$out"; then
		die "$label: spctl accepted this as Developer ID or notarized; this build has no such signature"
	fi
}

verify_bundle source "$app"
src_hash=$(cat "$work/source-cdhash.txt")

rm -rf "$dist"
mkdir -p "$dist"
ditto -c -k --keepParent "$app" "$zip"
hdiutil create -volname magpie -srcfolder "$app" -ov -format UDZO "$dmg" >/dev/null
hdiutil verify "$dmg" >/dev/null

ditto -x -k "$zip" "$work/zip"
verify_bundle zip "$work/zip/magpie.app"
[ "$(cat "$work/zip-cdhash.txt")" = "$src_hash" ] || die "zip CDHash differs from the bundle that was packed"

# The Actions artifact is another zip around this zip. ditto --keepParent
# on a file keeps that file's parent directory, so a zip that lives in
# dist/ extracts as dist/<name>.zip. Pack the file from a directory that
# contains only it, without --keepParent, so the archive root is the zip.
mkdir -p "$work/wrap-src"
cp "$zip" "$work/wrap-src/"
ditto -c -k "$work/wrap-src" "$work/outer.zip"
ditto -x -k "$work/outer.zip" "$work/outer"
cmp "$zip" "$work/outer/$(basename "$zip")" \
	|| die "wrapping the zip changed its bytes"
ditto -x -k "$work/outer/magpie-darwin-arm64-unsigned.zip" "$work/outer-app"
verify_bundle outer-zip "$work/outer-app/magpie.app"
[ "$(cat "$work/outer-zip-cdhash.txt")" = "$src_hash" ] || die "outer-zip CDHash differs"

mount=$(mktemp -d)
hdiutil attach "$dmg" -nobrowse -readonly -mountpoint "$mount" >/dev/null
verify_bundle dmg "$mount/magpie.app"
[ "$(cat "$work/dmg-cdhash.txt")" = "$src_hash" ] || die "dmg CDHash differs from the bundle that was packed"
hdiutil detach "$mount" >/dev/null
mount=

# spctl on the extracted app. This does not launch the GUI.
# The first assessment has no download mark. The second has the quarantine
# attribute Finder would set, on the bundle and on every file inside it.
xattr -d com.apple.quarantine "$work/zip/magpie.app" 2>/dev/null || true
assess plain "$work/zip/magpie.app"
q="0083;$(printf '%x' "$(date +%s)");pack-app;00000000-0000-0000-0000-000000000000"
xattr -w com.apple.quarantine "$q" "$work/zip/magpie.app"
find "$work/zip/magpie.app" -print > "$work/quarantine-files.txt"
while read -r f; do
	xattr -w com.apple.quarantine "$q" "$f" || true
done < "$work/quarantine-files.txt"
assess quarantined "$work/zip/magpie.app"

{
	echo "magpie.app ad-hoc seal. Not a Developer ID signature. Not notarized."
	echo "Checked the extracted zip, a second zip around that zip, and the mounted dmg."
	echo "CDHash=$src_hash"
	echo "codesign --verify --deep --strict passed on each. spctl rejection below is the unnotarized result."
	echo "No GUI launch was attempted."
	echo
	echo "==== extracted zip: codesign -dv --verbose=4 ===="
	cat "$work/zip-dv.txt"
	echo
	echo "==== extracted zip: codesign --verify --deep --strict --verbose=2 ===="
	cat "$work/zip-verify.txt"
	echo
	echo "==== spctl, no quarantine attribute ===="
	cat "$work/plain-spctl.txt"
	echo
	echo "==== spctl, com.apple.quarantine set on this app only ===="
	cat "$work/quarantined-spctl.txt"
	echo
	echo "==== shasum ===="
	shasum -a 256 "$zip" "$dmg"
} > "$report"

cat > "$dist/UNSIGNED.txt" <<'EOF'
这个 magpie.app 有临时签名（ad-hoc，codesign --sign -），用来保证安装包本身是完整的。
它没有 Developer ID 证书，也没有公证。macOS 不会把它当成已公证的应用。

请打开本目录里的 zip（用访达解压）或 dmg，把 magpie.app 拖到「应用程序」。
不要再用别的压缩工具重新打包 .app。

第一次打开时，系统会因为「无法验证开发者」而拦住。这是未公证应用的正常结果。
官方做法只针对这一个应用（https://support.apple.com/zh-cn/guide/mac-help/mh40616/mac）：
先打开一次，在提示上点「完成」，然后到「系统设置 → 隐私与安全性」里对 magpie 点「仍要打开」。
也可以按住 Control 再点 magpie.app，选「打开」。
不要关闭整台 Mac 的 Gatekeeper，也不要执行 spctl --master-disable。

若对话框仍是「已损坏，请移到废纸篓」，并且「隐私与安全性」里没有「仍要打开」，先看签名，不要先改系统策略：

  codesign --verify --deep --strict --verbose=2 /Applications/magpie.app
  codesign -dv --verbose=4 /Applications/magpie.app
  spctl --assess --verbose=4 --type execute /Applications/magpie.app

「valid on disk」且 Signature=adhoc、TeamIdentifier=not set，表示文件没有损坏。
这时 spctl 拒绝（no usable signature 一类）是未公证的预期结果，不是安装包坏了。
若签名完好但系统仍不提供「仍要打开」，只去掉这一个应用的下载标记：

  xattr -dr com.apple.quarantine /Applications/magpie.app

这不改变其他应用的 Gatekeeper。

This app is ad-hoc signed for bundle integrity. It is not Developer ID signed and not notarized.
EOF

echo "packed and verified $zip"
echo "packed and verified $dmg"
shasum -a 256 "$zip" "$dmg"
