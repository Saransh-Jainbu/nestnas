#!/usr/bin/env bash
# End-to-end smoke test: builds nothing, expects ./nestnas binary path in $1.
set -u
# SMOKE_ENCRYPT=1 runs the whole suite with at-rest encryption turned on (plus extra encryption checks).
BIN=${1:-./nestnas}; PORT=${PORT:-18080}; TP=$((PORT+1)); SP=$((PORT+2)); ENC=${SMOKE_ENCRYPT:-0}
T=$(mktemp -d); J="$T/jar"; B="http://127.0.0.1:$PORT"; PASS_PHRASE="correct horse battery staple"
start(){ NESTNAS_HOME="$T/home" "$BIN" serve --port $PORT --tls-port $TP --setup-port $SP --listen 127.0.0.1 >>"$T/log" 2>&1 & PID=$!; for i in $(seq 1 50); do curl -s -o /dev/null "$B/api/status" && break; sleep 0.2; done; }
start
trap 'kill $PID 2>/dev/null; wait $PID 2>/dev/null; rm -rf "$T"' EXIT
echo "=== mode: encryption $([ "$ENC" = 1 ] && echo ON || echo off) ==="
fail=0; ok(){ echo "PASS $1"; }; bad(){ echo "FAIL $1 ($2)"; fail=1; }
chk(){ [ "$2" = "$3" ] && ok "$1" || bad "$1" "got $2 want $3"; }
code(){ curl -s -o /dev/null -w '%{http_code}' "$@"; }
C="curl -s -b $J -c $J"

chk "status needs setup" "$(curl -s $B/api/status | grep -o '"setup_needed":true')" '"setup_needed":true'
chk "setup folder picker works locally" "$(curl -s "$B/api/setup/browse?path=$T" | grep -c '"writable":true')" 1
chk "folder picker rejects foreign Host header" "$(code -H 'Host: evil.example.com' "$B/api/setup/browse")" 421
chk "folder picker mkdir" "$(code -X POST -d "{\"path\":\"$T\",\"name\":\"picked\"}" $B/api/setup/mkdir)" 200
chk "folder picker mkdir bad name" "$(code -X POST -d "{\"path\":\"$T\",\"name\":\"../x\"}" $B/api/setup/mkdir)" 400
chk "unauth list blocked" "$(code $B/api/list)" 401
chk "setup short pw rejected" "$(code -X POST -d '{"username":"admin","password":"x","quota_gb":1}' $B/api/setup)" 400
ENCJSON=""; [ "$ENC" = 1 ] && ENCJSON=",\"encrypt\":true,\"passphrase\":\"$PASS_PHRASE\""
chk "setup ok" "$(curl -s -o $T/setup.json -w '%{http_code}' -c $J -X POST -d "{\"username\":\"admin\",\"password\":\"password123\",\"quota_gb\":0.001,\"storage_path\":\"$T/store\"$ENCJSON}" $B/api/setup)" 200
chk "folder picker closed after setup" "$(code "$B/api/setup/browse")" 403
chk "setup twice blocked" "$(code -X POST -d '{}' $B/api/setup)" 409
chk "mkdir" "$(code -b $J -X POST -d '{"path":"/docs"}' $B/api/mkdir)" 200
echo hello > "$T/f.txt"
chk "upload" "$(code -b $J -X PUT --data-binary @$T/f.txt "$B/api/upload?path=/docs&name=f.txt")" 200
chk "download content" "$($C "$B/api/download?path=/docs/f.txt")" hello
chk "traversal upload name" "$(code -b $J -X PUT --data-binary @$T/f.txt "$B/api/upload?path=/&name=../x")" 400
chk "traversal download" "$(code -b $J "$B/api/download?path=/../../../../etc/passwd")" 404
head -c 2000000 /dev/zero > "$T/big"
chk "quota enforced (1MB cap)" "$(code -b $J -X PUT --data-binary @$T/big "$B/api/upload?path=/&name=big")" 413
chk "cross-origin POST blocked" "$(code -b $J -X POST -H 'Origin: http://evil.com' -d '{"path":"/z"}' $B/api/mkdir)" 403
SH=$($C -X POST -d '{"path":"/docs/f.txt","hours":1}' $B/api/share | sed 's/.*"url":"\([^"]*\)".*/\1/')
chk "public share" "$(curl -s $B$SH)" hello
chk "add user" "$(code -b $J -X POST -d '{"name":"bob","password":"bobpass123","quota_gb":1}' $B/api/users/add)" 200
BJ="$T/bob"
chk "bob login" "$(code -c $BJ -X POST -d '{"username":"bob","password":"bobpass123"}' $B/api/login)" 200
chk "bob cannot see admin files" "$(curl -s -b $BJ "$B/api/list?path=/docs" | grep -c f.txt)" 0
chk "bob not admin" "$(code -b $BJ $B/api/users)" 403
chk "bad login" "$(code -X POST -d '{"username":"bob","password":"nope"}' $B/api/login)" 401
mkdir -p "$T/store/users/admin/site"; echo '<h1>hi</h1>' > "$T/store/users/admin/site/index.html"
chk "static site" "$(curl -s $B/sites/admin/)" '<h1>hi</h1>'
chk "site CSP sandbox" "$(curl -sI $B/sites/admin/ | grep -ci 'content-security-policy: sandbox')" 1
chk "system info" "$(code -b $J $B/api/system)" 200
chk "apps list" "$(code -b $J $B/api/apps)" 200
chk "last admin protected" "$(code -b $J -X POST -d '{"name":"admin"}' $B/api/users/delete)" 400
# --- trash & zip ---
echo "zipme" > "$T/z.txt"
$C -X POST -d '{"path":"/zipdir"}' $B/api/mkdir >/dev/null
$C -X PUT --data-binary @$T/z.txt "$B/api/upload?path=/zipdir&name=z.txt" >/dev/null
$C "$B/api/zip?path=/zipdir" -o "$T/out.zip"
chk "zip folder is a valid archive" "$(python3 -c "import zipfile,sys;z=zipfile.ZipFile('$T/out.zip');print(z.read('z.txt').decode().strip())")" zipme
chk "delete moves to trash" "$(code -b $J -X POST -d '{"path":"/zipdir/z.txt"}' $B/api/delete)" 200
chk "deleted file gone from folder" "$($C "$B/api/list?path=/zipdir" | grep -c z.txt)" 0
chk "trash lists it" "$($C $B/api/trash | grep -c '"name":"z.txt"')" 1
chk "trash folder hidden from root list" "$($C "$B/api/list?path=/" | grep -c '.trash')" 0
chk "cannot browse trash directly" "$(code -b $J "$B/api/list?path=/.trash")" 400
chk "cannot delete trash dir" "$(code -b $J -X POST -d '{"path":"/.trash"}' $B/api/delete)" 400
TID=$($C $B/api/trash | sed 's/.*"id":"\([^"]*\)".*/\1/')
chk "restore" "$(code -b $J -X POST -d "{\"id\":\"$TID\"}" $B/api/trash/restore)" 200
chk "restored file back" "$($C "$B/api/download?path=/zipdir/z.txt")" zipme
chk "bad trash id rejected" "$(code -b $J -X POST -d '{"id":"../../etc"}' $B/api/trash/restore)" 400
$C -X POST -d '{"path":"/zipdir/z.txt"}' $B/api/delete >/dev/null
chk "empty trash" "$(code -b $J -X POST -d '{}' $B/api/trash/empty)" 200
chk "trash now empty" "$($C $B/api/trash | grep -c '"name"')" 0
# --- WebDAV network drive ---
D="curl -s -u admin:password123"
dcode(){ curl -s -o /dev/null -w '%{http_code}' -u admin:password123 "$@"; }
chk "dav: options without login" "$(curl -sI -X OPTIONS $B/dav/ | grep -ci '^dav: 1')" 1
chk "dav: unauthenticated -> 401" "$(code -X PROPFIND $B/dav/)" 401
chk "dav: 401 asks for Basic auth" "$(curl -sI -X PROPFIND $B/dav/ | grep -ci 'www-authenticate: basic')" 1
chk "dav: wrong password -> 401" "$(code -u admin:wrong -X PROPFIND $B/dav/)" 401
chk "dav: propfind root" "$(dcode -X PROPFIND -H 'Depth: 0' $B/dav/)" 207
chk "dav: mkcol" "$(dcode -X MKCOL $B/dav/davdir)" 201
chk "dav: mkcol twice -> 405" "$(dcode -X MKCOL $B/dav/davdir)" 405
chk "dav: mkcol missing parent -> 409" "$(dcode -X MKCOL $B/dav/nope/child)" 409
echo "dav-data" > "$T/d.txt"
chk "dav: put new -> 201" "$(dcode -X PUT --data-binary @$T/d.txt $B/dav/davdir/d%20one.txt)" 201
chk "dav: put overwrite -> 204" "$(dcode -X PUT --data-binary @$T/d.txt $B/dav/davdir/d%20one.txt)" 204
chk "dav: get content" "$($D $B/dav/davdir/d%20one.txt)" dav-data
chk "dav: range request" "$($D -H 'Range: bytes=0-2' $B/dav/davdir/d%20one.txt)" dav
chk "dav: propfind lists file (escaped href)" "$($D -X PROPFIND -H 'Depth: 1' $B/dav/davdir/ | grep -c 'd%20one.txt')" 1
chk "dav: quota props present" "$($D -X PROPFIND -H 'Depth: 0' $B/dav/ | grep -c 'quota-available-bytes')" 1
chk "dav: visible in web UI" "$($C "$B/api/list?path=/davdir" | grep -c 'd one.txt')" 1
chk "dav: copy -> 201" "$(dcode -X COPY -H "Destination: $B/dav/davdir/copy.txt" $B/dav/davdir/d%20one.txt)" 201
chk "dav: copy content" "$($D $B/dav/davdir/copy.txt)" dav-data
chk "dav: copy overwrite=F -> 412" "$(dcode -X COPY -H 'Overwrite: F' -H "Destination: $B/dav/davdir/copy.txt" $B/dav/davdir/d%20one.txt)" 412
chk "dav: move -> 201" "$(dcode -X MOVE -H "Destination: $B/dav/davdir/moved.txt" $B/dav/davdir/copy.txt)" 201
chk "dav: moved source gone" "$(dcode $B/dav/davdir/copy.txt)" 404
chk "dav: move to foreign host rejected" "$(dcode -X MOVE -H 'Destination: http://evil.com/dav/x' $B/dav/davdir/moved.txt)" 400
chk "dav: move folder into itself rejected" "$(dcode -X MOVE -H "Destination: $B/dav/davdir/sub" $B/dav/davdir)" 403
chk "dav: lock gives token" "$(curl -sI -u admin:password123 -X LOCK $B/dav/davdir/moved.txt | grep -ci '^lock-token:')" 1
chk "dav: delete -> 204" "$(dcode -X DELETE $B/dav/davdir/moved.txt)" 204
chk "dav: delete went to trash" "$($C $B/api/trash | grep -c 'moved.txt')" 1
chk "dav: cannot delete root" "$(dcode -X DELETE $B/dav/)" 403
chk "dav: trash not reachable" "$(dcode -X PROPFIND $B/dav/.trash/)" 404
chk "dav: traversal leaks nothing" "$($D -L --path-as-is $B/dav/../../../etc/passwd | grep -c 'root:')" 0
chk "dav: encoded traversal leaks nothing" "$($D -L --path-as-is "$B/dav/%2e%2e/%2e%2e/etc/passwd" | grep -c 'root:')" 0
chk "dav: other user cannot see my files" "$(curl -s -u bob:bobpass123 -X PROPFIND -H 'Depth: 1' $B/dav/davdir/ | grep -c 'd%20one.txt')" 0
chk "dav: quota enforced (1MB cap)" "$(dcode -X PUT --data-binary @$T/big $B/dav/big.bin)" 507
chk "dav: no partial file left after quota fail" "$($D -X PROPFIND -H 'Depth: 1' $B/dav/ | grep -c 'big.bin\|\.part')" 0
# --- unplugged storage drive must NOT fall back to the internal disk ---
chk "storage has an identity marker" "$([ -s "$T/store/.nestnas-id" ] && echo yes || echo no)" yes
mv "$T/store/users" "$T/store/users.off"
chk "drive gone: web API says 503" "$(code -b $J $B/api/me)" 503
chk "drive gone: upload refused" "$(code -b $J -X PUT --data-binary @$T/f.txt "$B/api/upload?path=/&name=x.txt")" 503
chk "drive gone: dav refused" "$(dcode -X PUT --data-binary @$T/d.txt $B/dav/x.txt)" 503
chk "drive gone: share link refused" "$(code $B$SH)" 503
chk "drive gone: status reports it" "$(curl -s $B/api/status | grep -c '"storage_ok":false')" 1
chk "drive gone: nothing recreated on internal disk" "$([ -e "$T/store/users" ] && echo recreated || echo clean)" clean
sleep 2.2   # storage status is cached for 2s
mv "$T/store/users.off" "$T/store/users"
chk "drive back: works again, no restart needed" "$(code -b $J $B/api/me)" 200
# a DIFFERENT drive at the same path (has a users folder but not our marker) must be refused
mv "$T/store" "$T/store.real"; mkdir -p "$T/store/users"; sleep 2.2
chk "wrong drive at same path: refused" "$(code -b $J $B/api/me)" 503
rm -rf "$T/store"; mv "$T/store.real" "$T/store"; sleep 2.2
chk "original drive back: works again" "$(code -b $J $B/api/me)" 200
# --- in-app preview: /api/view and /api/thumb ---
python3 - "$T" <<'PY'
import sys, zlib, struct
t = sys.argv[1]
def chunk(k, d):
    c = struct.pack('>I', len(d)) + k + d
    return c + struct.pack('>I', zlib.crc32(k + d) & 0xffffffff)
w, h = 300, 200
import os
raw = b''.join(b'\x00' + os.urandom(w * 3) for _ in range(h))   # noisy like a real photo, so it does not compress away
open(t + '/pic.png', 'wb').write(b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', w, h, 8, 2, 0, 0, 0)) + chunk(b'IDAT', zlib.compress(raw)) + chunk(b'IEND', b''))
PY
echo '<script>alert(document.cookie)</script>' > "$T/evil.html"; cp "$T/evil.html" "$T/evil.svg"
echo 'plain notes' > "$T/notes.txt"; head -c 5000 /dev/urandom > "$T/clip.mp4"; echo '%PDF-1.4 test' > "$T/doc.pdf"
for f in pic.png evil.html evil.svg notes.txt clip.mp4 doc.pdf; do $C -X PUT --data-binary @$T/$f "$B/api/upload?path=/&name=$f" >/dev/null; done
hdr(){ curl -s -D - -o /dev/null -b $J "$@"; }
chk "view: unauthenticated -> 401" "$(code "$B/api/view?path=/pic.png")" 401
chk "view: png has the right content type" "$(hdr "$B/api/view?path=/pic.png" | grep -ci '^content-type: image/png')" 1
chk "view: png bytes are identical to the upload" "$(curl -s -b $J "$B/api/view?path=/pic.png" | cmp -s - $T/pic.png && echo same || echo different)" same
chk "view: never cached by the browser" "$(hdr "$B/api/view?path=/pic.png" | grep -ci '^cache-control: no-store')" 1
chk "view: sandboxed by CSP" "$(hdr "$B/api/view?path=/pic.png" | grep -ci '^content-security-policy: sandbox')" 1
chk "view: may be framed by our own page only" "$(hdr "$B/api/view?path=/doc.pdf" | grep -ci '^x-frame-options: sameorigin')" 1
chk "view: html refused (can run script)" "$(code -b $J "$B/api/view?path=/evil.html")" 415
chk "view: svg refused (can run script)" "$(code -b $J "$B/api/view?path=/evil.svg")" 415
chk "view: text served as text/plain" "$(hdr "$B/api/view?path=/notes.txt" | grep -ci '^content-type: text/plain')" 1
chk "view: pdf served as application/pdf" "$(hdr "$B/api/view?path=/doc.pdf" | grep -ci '^content-type: application/pdf')" 1
chk "view: range request -> 206 (video seeking)" "$(code -b $J -H 'Range: bytes=0-99' "$B/api/view?path=/clip.mp4")" 206
chk "view: range returns exactly the bytes asked for" "$(curl -s -b $J -H 'Range: bytes=0-99' "$B/api/view?path=/clip.mp4" | wc -c)" 100
chk "view: range content matches the original" "$(curl -s -b $J -H 'Range: bytes=10-19' "$B/api/view?path=/clip.mp4" | cmp -s - <(tail -c +11 $T/clip.mp4 | head -c 10) && echo same || echo different)" same
chk "view: traversal stays in the jail" "$(code -b $J "$B/api/view?path=/../../../../etc/hostname.txt")" 404
chk "view: another user cannot see my file" "$(code -b $BJ "$B/api/view?path=/pic.png")" 404
chk "thumb: unauthenticated -> 401" "$(code "$B/api/thumb?path=/pic.png")" 401
chk "thumb: png -> jpeg" "$(hdr "$B/api/thumb?path=/pic.png&w=100" | grep -ci '^content-type: image/jpeg')" 1
chk "thumb: valid JPEG bytes" "$(curl -s -b $J "$B/api/thumb?path=/pic.png&w=100" | head -c2 | od -An -tx1 | tr -d ' \n')" ffd8
chk "thumb: smaller than the original image" "$([ $(curl -s -b $J "$B/api/thumb?path=/pic.png&w=64" | wc -c) -lt $(wc -c < $T/pic.png) ] && echo smaller || echo bigger)" smaller
chk "thumb: absurd size is clamped, not an error" "$(code -b $J "$B/api/thumb?path=/pic.png&w=99999")" 200
chk "thumb: text file has no thumbnail" "$(code -b $J "$B/api/thumb?path=/notes.txt")" 415
chk "thumb: html has no thumbnail" "$(code -b $J "$B/api/thumb?path=/evil.html")" 415
chk "thumb: fake image is refused, not crashed" "$(code -b $J "$B/api/thumb?path=/clip.mp4")" 415

# --- HTTPS (TLS 1.3) ---
CA="$T/home/tls/ca.crt"; S="https://127.0.0.1:$TP"
chk "tls: local CA created" "$([ -s "$CA" ] && echo yes || echo no)" yes
chk "tls: CA private key is owner-only" "$(stat -c %a "$T/home/tls/ca.key")" 600
chk "tls: trusted client works" "$(curl -s --cacert $CA $S/api/status | grep -c setup_needed)" 1
chk "tls: client without the CA is refused" "$(code $S/api/status)" 000
chk "tls: TLS 1.2 is refused" "$(code --cacert $CA --tls-max 1.2 $S/api/status)" 000
chk "tls: HSTS header sent" "$(curl -sI --cacert $CA $S/api/status | grep -ci '^strict-transport-security')" 1
chk "tls: session cookie is Secure" "$(curl -s -D - -o /dev/null --cacert $CA -X POST -d '{"username":"admin","password":"password123"}' $S/api/login | grep -i '^set-cookie' | grep -ci 'secure')" 1
chk "tls: CA certificate downloadable" "$(curl -s --cacert $CA $S/ca.crt | grep -c 'BEGIN CERTIFICATE')" 1
chk "tls: iPhone profile has the Apple profile content type" "$(curl -sI --cacert $CA $S/NestNAS.mobileconfig | grep -ci '^content-type: application/x-apple-aspen-config')" 1
chk "tls: iPhone profile is not forced to download as an attachment" "$(curl -sI --cacert $CA $S/NestNAS.mobileconfig | grep -ci '^content-disposition')" 0
curl -s --cacert $CA $S/NestNAS.mobileconfig -o "$T/p.mobileconfig"
chk "tls: iPhone profile is a valid Apple plist with a root-cert payload" "$(python3 -c "
import plistlib
p = plistlib.load(open('$T/p.mobileconfig','rb'))
c = p['PayloadContent'][0]
print(p['PayloadType'], c['PayloadType'], len(c['PayloadContent']) > 300)")" "Configuration com.apple.security.root True"
python3 -c "
import plistlib
open('$T/from-profile.der','wb').write(plistlib.load(open('$T/p.mobileconfig','rb'))['PayloadContent'][0]['PayloadContent'])"
chk "tls: certificate inside the profile is our CA" "$(openssl x509 -inform DER -in $T/from-profile.der -noout -subject | grep -c 'NestNAS Local CA')" 1
chk "tls: profile certificate matches the served CA" "$(openssl x509 -inform DER -in $T/from-profile.der -noout -fingerprint -sha256)" "$(openssl x509 -in $CA -noout -fingerprint -sha256)"
chk "tls: DER certificate download parses" "$(curl -s --cacert $CA $S/ca.cer | openssl x509 -inform DER -noout -subject | grep -c 'NestNAS Local CA')" 1
chk "tls: PEM certificate is served inline (iOS Safari)" "$(curl -sI --cacert $CA $S/ca.crt | grep -ci '^content-disposition')" 0
chk "tls: profile never contains a private key" "$(grep -c 'PRIVATE' $T/p.mobileconfig)" 0
chk "tls: CA download never contains a private key" "$(curl -s --cacert $CA $S/ca.crt | grep -c 'PRIVATE')" 0
chk "tls: WebDAV works over HTTPS" "$(curl -s -o /dev/null -w '%{http_code}' --cacert $CA -u admin:password123 -X PROPFIND -H 'Depth: 0' $S/dav/)" 207
chk "http: foreign Host header refused on the plain port" "$(code -H 'Host: evil.example.com' $B/api/status)" 421

# --- phone / computer backup ---
urlenc(){ python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "$1"; }
BDIR="/Phone Backup/iPhone/2024/05"; BDIRQ=$(urlenc "$BDIR")
printf 'photo-one-bytes' > "$T/b1.jpg"
chk "backup: upload creates the device/year/month folders" "$(code -b $J -X PUT --data-binary @$T/b1.jpg "$B/api/upload?backup=1&path=$BDIRQ&name=IMG_0001.JPG&mtime=1700000000")" 200
chk "backup: file is listed in the date folder" "$($C "$B/api/list?path=$BDIRQ" | grep -c IMG_0001.JPG)" 1
chk "backup: file keeps the photo's own date" "$($C "$B/api/list?path=$BDIRQ" | grep -c '"mtime":1700000000')" 1
chk "backup: unauthenticated check refused" "$(code -X POST -d '{"files":[]}' $B/api/backup/check)" 401
CHECK="{\"files\":[{\"path\":\"$BDIR/IMG_0001.JPG\",\"size\":15},{\"path\":\"$BDIR/IMG_0002.JPG\",\"size\":15},{\"path\":\"$BDIR/IMG_0001.JPG\",\"size\":99}]}"
chk "backup: check reports what is missing (absent, or different size)" "$($C -X POST -d "$CHECK" $B/api/backup/check | tr -d ' ')" '{"have":1,"missing":[1,2]}'
chk "backup: re-sending the same photo is skipped" "$($C -X PUT --data-binary @$T/b1.jpg "$B/api/upload?backup=1&path=$BDIRQ&name=IMG_0001.JPG" | grep -c '"skipped":true')" 1
printf 'a-DIFFERENT-photo-with-the-same-name' > "$T/b2.jpg"
chk "backup: same name but different photo is kept as a second file" "$($C -X PUT --data-binary @$T/b2.jpg "$B/api/upload?backup=1&path=$BDIRQ&name=IMG_0001.JPG" | grep -c 'IMG_0001 (2).JPG')" 1
chk "backup: the original photo was NOT overwritten" "$($C "$B/api/download?path=$BDIRQ/IMG_0001.JPG")" photo-one-bytes
chk "backup: the second photo is intact too" "$($C "$B/api/download?path=$BDIRQ/IMG_0001%20(2).JPG")" a-DIFFERENT-photo-with-the-same-name
chk "backup: bad file name refused" "$(code -b $J -X PUT --data-binary @$T/b1.jpg "$B/api/upload?backup=1&path=$BDIRQ&name=../x")" 400
chk "backup: path traversal stays inside the user's folder" "$(code -b $J -X PUT --data-binary @$T/b1.jpg "$B/api/upload?backup=1&path=$(urlenc /../../../../tmp/nn-evil-backup)&name=x.jpg")" 200
chk "backup: nothing was created outside the NAS" "$([ -e /tmp/nn-evil-backup ] && echo escaped || echo safe)" safe
chk "backup: it landed inside the user's own folder instead" "$([ -f "$T/store/users/admin/tmp/nn-evil-backup/x.jpg" ] && echo yes || echo no)" yes
chk "backup: another user does not see my backup" "$(curl -s -b $BJ -X POST -d "$CHECK" $B/api/backup/check | grep -c '"have":0')" 1
chk "backup: absurdly large check is refused" "$(python3 -c "import json;print(json.dumps({'files':[{'path':'/a','size':1}]*4001}))" | curl -s -o /dev/null -w '%{http_code}' -b $J -X POST -d @- $B/api/backup/check)" 400
chk "backup: over-quota upload is refused" "$(code -b $J -X PUT --data-binary @$T/big "$B/api/upload?backup=1&path=$BDIRQ&name=huge.mov")" 413
chk "backup: no half-written file left after a refused upload" "$($C "$B/api/list?path=$BDIRQ" | grep -c 'huge\|\.part')" 0
# --- installable web app (Add to Home Screen) ---
chk "pwa: manifest is served with the manifest type" "$(curl -sI $B/manifest.webmanifest | grep -ci '^content-type: application/manifest+json')" 1
chk "pwa: manifest declares standalone display and an icon" "$(curl -s $B/manifest.webmanifest | python3 -c "import sys,json;m=json.load(sys.stdin);print(m['display'],m['icons'][0]['src'])")" "standalone /icon.png"
chk "pwa: icon is a PNG" "$(curl -s $B/apple-touch-icon.png | head -c4 | od -An -c | tr -d ' \n')" '211PNG'
chk "pwa: dashboard links the manifest and touch icon" "$(curl -s $B/ | grep -c 'rel="manifest"\|rel="apple-touch-icon"')" 2
# --- embedded fonts and a self-contained dashboard ---
chk "fonts: served from the program itself" "$(code $B/fonts/fraunces.woff2)" 200
chk "fonts: correct content type" "$(curl -sI $B/fonts/fraunces.woff2 | grep -ci '^content-type: font/woff2')" 1
chk "fonts: cached for a year" "$(curl -sI $B/fonts/geist.woff2 | grep -ci 'cache-control: public, max-age=31536000, immutable')" 1
chk "fonts: all six files present" "$(for f in fraunces fraunces-italic instrument-serif instrument-serif-italic geist geist-mono; do curl -s -o /dev/null -w '%{http_code}\n' $B/fonts/$f.woff2; done | grep -c 200)" 6
chk "fonts: a real woff2 file" "$(curl -s $B/fonts/geist.woff2 | head -c4)" wOF2
chk "fonts: missing file -> 404" "$(code $B/fonts/nope.woff2)" 404
chk "fonts: only .woff2 is served" "$(code $B/fonts/geist.woff2.txt)" 404
chk "fonts: no directory listing" "$(code $B/fonts/)" 404
chk "fonts: traversal cannot reach other files" "$(code -L --path-as-is "$B/fonts/../index.html")" 404
chk "dashboard: CSP allows data: images (grain) and nothing external" "$(curl -sI $B/ | grep -i '^content-security-policy' | grep -c "img-src 'self' data:")" 1
chk "dashboard: loads nothing from other websites" "$(curl -s $B/ | grep -cE '<(link|script|img|iframe|source)[^>]*(src|href)="https?://|url\(https?://|@import')" 0
chk "dashboard: fonts are referenced locally" "$(curl -s $B/ | grep -c 'url(/fonts/')" 6
# --- brand assets ---
chk "brand: favicon is an SVG" "$(curl -sI $B/favicon.svg | grep -ci '^content-type: image/svg+xml')" 1
chk "brand: favicon contains the logo" "$(curl -s $B/favicon.svg | grep -c 'aria-label="NestNAS"')" 1
chk "brand: 512px icon is a PNG" "$(curl -s $B/icon-512.png | head -c4 | od -An -c | tr -d ' \n')" '211PNG'
chk "brand: 256px icon is a PNG" "$(curl -s $B/icon.png | head -c4 | od -An -c | tr -d ' \n')" '211PNG'
chk "brand: manifest lists both icon sizes" "$(curl -s $B/manifest.webmanifest | python3 -c "import sys,json;print(len(json.load(sys.stdin)['icons']))")" 2
chk "brand: dashboard links the favicon" "$(curl -s $B/ | grep -c 'rel="icon" href="/favicon.svg"')" 1
chk "brand: dashboard uses the black and arctic-blue palette" "$(curl -s $B/ | grep -c -- '--accent:#6dc8ff')" 1
chk "dashboard: no iCloud references" "$(curl -s $B/ | grep -ci icloud)" 0
# --- device-setup port (plain HTTP, public certificate ONLY) ---
D8="http://127.0.0.1:$SP"
chk "setup port: landing page loads with the fingerprint" "$(curl -s $D8/ | grep -c "$(openssl x509 -in $CA -noout -fingerprint -sha256 | cut -d= -f2)")" 1
chk "setup port: iPhone profile downloads over plain HTTP" "$(code $D8/NestNAS.mobileconfig)" 200
curl -s $D8/NestNAS.mobileconfig -o "$T/p2.mobileconfig"
chk "setup port: that profile is a valid Apple plist" "$(python3 -c "import plistlib;print(plistlib.load(open('$T/p2.mobileconfig','rb'))['PayloadContent'][0]['PayloadType'])")" com.apple.security.root
chk "setup port: certificate downloads" "$(curl -s $D8/ca.crt | grep -c 'BEGIN CERTIFICATE')" 1
for path in /api/status /api/login /api/setup/browse /dav/ /s/abc /sites/admin/ /index.html "/api/download?path=/pic.png"; do
  chk "setup port: $path is NOT reachable" "$(code "$D8$path")" 404
done
chk "setup port: cannot write (POST refused)" "$(code -X POST -d '{}' $D8/api/login)" 405
chk "setup port: WebDAV verbs refused" "$(code -X PROPFIND $D8/dav/)" 405
# --- at-rest encryption ---
if [ "$ENC" = 1 ]; then
  REC=$(sed -n 's/.*"recovery_key":"\([^"]*\)".*/\1/p' "$T/setup.json")
  chk "enc: recovery key issued at setup" "$([ ${#REC} -ge 50 ] && echo yes || echo no)" yes
  chk "enc: key file stored on the drive" "$([ -s "$T/store/.nestnas-vault" ] && echo yes || echo no)" yes
  chk "enc: master key never written in the clear" "$(grep -c 'BEGIN\|passphrase.*correct' "$T/store/.nestnas-vault")" 0
  echo "TOP-SECRET-PLAINTEXT-MARKER-12345" > "$T/sec.txt"
  chk "enc: web upload" "$(code -b $J -X PUT --data-binary @$T/sec.txt "$B/api/upload?path=/&name=sec.txt")" 200
  RAW="$T/store/users/admin/sec.txt"
  chk "enc: file on disk has the NNE1 header" "$(head -c4 "$RAW")" NNE1
  chk "enc: plaintext is NOT on disk" "$(grep -c TOP-SECRET "$RAW")" 0
  chk "enc: web download decrypts" "$($C "$B/api/download?path=/sec.txt")" TOP-SECRET-PLAINTEXT-MARKER-12345
  chk "enc: listed size is the real (plaintext) size" "$($C "$B/api/list?path=/" | grep -o '"name":"sec.txt"' | wc -l)$($C "$B/api/list?path=/" | grep -o '"size":34' | wc -l)" 11
  chk "enc: range request decrypts" "$($C -H 'Range: bytes=4-9' "$B/api/download?path=/sec.txt")" SECRET
  chk "enc: WebDAV download decrypts" "$($D $B/dav/sec.txt)" TOP-SECRET-PLAINTEXT-MARKER-12345
  chk "enc: WebDAV reports the real size" "$($D -X PROPFIND -H 'Depth: 1' $B/dav/ | grep -c '<D:getcontentlength>34<')" 1
  echo "uploaded-over-webdav-marker-777" > "$T/dv.txt"
  $D -X PUT --data-binary @$T/dv.txt $B/dav/dv.txt -o /dev/null
  chk "enc: WebDAV upload is encrypted on disk" "$(grep -c uploaded-over-webdav "$T/store/users/admin/dv.txt")" 0
  chk "enc: WebDAV upload reads back" "$($D $B/dav/dv.txt)" uploaded-over-webdav-marker-777
  $C -X POST -d '{"path":"/davdir2"}' $B/api/mkdir >/dev/null; $C -X PUT --data-binary @$T/dv.txt "$B/api/upload?path=/davdir2&name=in.txt" >/dev/null
  $C "$B/api/zip?path=/davdir2" -o "$T/enc.zip"
  chk "enc: zip of a folder contains decrypted data" "$(python3 -c "import zipfile;print(zipfile.ZipFile('$T/enc.zip').read('in.txt').decode().strip())")" uploaded-over-webdav-marker-777
  ESH=$($C -X POST -d '{"path":"/sec.txt","hours":1}' $B/api/share | sed 's/.*"url":"\([^"]*\)".*/\1/')
  chk "enc: public share link serves decrypted data" "$(curl -s $B$ESH)" TOP-SECRET-PLAINTEXT-MARKER-12345
  chk "enc: copy keeps working (raw ciphertext copy)" "$(dcode -X COPY -H "Destination: $B/dav/sec-copy.txt" $B/dav/sec.txt)" 201
  chk "enc: the copy decrypts" "$($D $B/dav/sec-copy.txt)" TOP-SECRET-PLAINTEXT-MARKER-12345
  chk "enc: status: encrypted and unlocked" "$(curl -s $B/api/status | grep -c '"encrypted":true,')$(curl -s $B/api/status | grep -c '"locked":false')" 11

  # lock / unlock
  chk "enc: lock now" "$(code -b $J -X POST -d '{}' $B/api/vault/lock)" 200
  chk "enc: locked -> file names still listed (no key needed)" "$(code -b $J "$B/api/list?path=/")" 200
  chk "enc: locked -> listing exposes no contents" "$(curl -s -b $J "$B/api/list?path=/" | grep -c TOP-SECRET-PLAINTEXT)" 0
  chk "enc: locked -> preview refused (423)" "$(code -b $J "$B/api/view?path=/pic.png")" 423
  chk "enc: locked -> thumbnail refused (423)" "$(code -b $J "$B/api/thumb?path=/pic.png")" 423
  chk "enc: locked -> download refused (423)" "$(code -b $J "$B/api/download?path=/sec.txt")" 423
  chk "enc: locked -> upload refused (423)" "$(code -b $J -X PUT --data-binary @$T/sec.txt "$B/api/upload?path=/&name=x.txt")" 423
  chk "enc: locked -> WebDAV refused (423)" "$(dcode $B/dav/sec.txt)" 423
  chk "enc: locked -> share link refused (423)" "$(code $B$ESH)" 423
  chk "enc: locked -> status says locked" "$(curl -s $B/api/status | grep -c '"locked":true')" 1
  chk "enc: locked -> dashboard calls still work" "$(code -b $J $B/api/me)" 200
  chk "enc: wrong passphrase rejected" "$(code -b $J -X POST -d '{"secret":"definitely not it"}' $B/api/vault/unlock)" 401
  chk "enc: unlock with passphrase" "$(code -b $J -X POST -d "{\"secret\":\"$PASS_PHRASE\"}" $B/api/vault/unlock)" 200
  chk "enc: image on disk is encrypted yet previews as a real PNG" "$(head -c4 "$T/store/users/admin/pic.png")$(curl -s -b $J "$B/api/view?path=/pic.png" | head -c4 | od -An -c | tr -d ' \n')" 'NNE1211PNG' 
  chk "enc: data intact after unlock" "$($C "$B/api/download?path=/sec.txt")" TOP-SECRET-PLAINTEXT-MARKER-12345
  chk "enc: non-admin cannot unlock" "$(curl -s -o /dev/null -w '%{http_code}' -u bob:bobpass123 -c $T/bobjar -X POST -d '{"username":"bob","password":"bobpass123"}' $B/api/login)$(code -b $T/bobjar -X POST -d '{"secret":"x"}' $B/api/vault/unlock)" 200403

  # restart: the key lives only in memory, so the server must come back locked
  kill $PID; wait $PID 2>/dev/null; start
  $C -X POST -d '{"username":"admin","password":"password123"}' $B/api/login >/dev/null
  chk "enc: after restart the storage is locked" "$(code -b $J "$B/api/download?path=/sec.txt")" 423
  chk "enc: recovery key unlocks" "$(code -b $J -X POST -d "{\"secret\":\"$REC\"}" $B/api/vault/unlock)" 200
  chk "enc: data intact after restart" "$($C "$B/api/download?path=/sec.txt")" TOP-SECRET-PLAINTEXT-MARKER-12345

  # change the passphrase
  chk "enc: change passphrase (too short refused)" "$(code -b $J -X POST -d "{\"old\":\"$PASS_PHRASE\",\"new\":\"short\"}" $B/api/vault/passphrase)" 400
  chk "enc: change passphrase (wrong old refused)" "$(code -b $J -X POST -d '{"old":"nope nope nope nope","new":"a brand new passphrase!"}' $B/api/vault/passphrase)" 403
  chk "enc: change passphrase" "$(code -b $J -X POST -d "{\"old\":\"$PASS_PHRASE\",\"new\":\"a brand new passphrase!\"}" $B/api/vault/passphrase)" 200
  $C -X POST -d '{}' $B/api/vault/lock >/dev/null
  chk "enc: old passphrase no longer works" "$(code -b $J -X POST -d "{\"secret\":\"$PASS_PHRASE\"}" $B/api/vault/unlock)" 401
  chk "enc: new passphrase works" "$(code -b $J -X POST -d '{"secret":"a brand new passphrase!"}' $B/api/vault/unlock)" 200
  chk "enc: files survive a passphrase change" "$($C "$B/api/download?path=/sec.txt")" TOP-SECRET-PLAINTEXT-MARKER-12345

  # tampering with the file on disk must be detected, never returned as garbage
  cp "$RAW" "$T/sec.bak"; printf '\xff' | dd of="$RAW" bs=1 seek=60 count=1 conv=notrunc 2>/dev/null
  chk "enc: tampered file is not served as valid data" "$($C "$B/api/download?path=/sec.txt" | grep -c TOP-SECRET)" 0
  cp "$T/sec.bak" "$RAW"
  chk "enc: restored file reads again" "$($C "$B/api/download?path=/sec.txt")" TOP-SECRET-PLAINTEXT-MARKER-12345

  # optional auto-unlock
  chk "enc: auto-unlock needs the passphrase" "$(code -b $J -X POST -d '{"enable":true,"secret":"wrong wrong wrong"}' $B/api/vault/autounlock)" 403
  chk "enc: enable auto-unlock" "$(code -b $J -X POST -d '{"enable":true,"secret":"a brand new passphrase!"}' $B/api/vault/autounlock)" 200
  chk "enc: auto-unlock key file is owner-only" "$(stat -c %a "$T/home/vault-auto.key")" 600
  kill $PID; wait $PID 2>/dev/null; start
  $C -X POST -d '{"username":"admin","password":"password123"}' $B/api/login >/dev/null
  chk "enc: auto-unlock: restart comes back unlocked" "$($C "$B/api/download?path=/sec.txt")" TOP-SECRET-PLAINTEXT-MARKER-12345
  chk "enc: disable auto-unlock" "$(code -b $J -X POST -d '{"enable":false}' $B/api/vault/autounlock)" 200
  chk "enc: auto-unlock key file removed" "$([ -e "$T/home/vault-auto.key" ] && echo present || echo gone)" gone
fi

# --- turning encryption on and off for an existing NAS (plain-mode run only) ---
if [ "$ENC" = 0 ]; then
  waitconv(){ for i in $(seq 1 40); do [ "$(curl -s -b $J $B/api/vault | grep -c '"running":false')" = 1 ] && return; sleep 0.5; done; }
  echo "convert-marker-AAA" > "$T/cv.txt"; $C -X PUT --data-binary @$T/cv.txt "$B/api/upload?path=/&name=cv.txt" >/dev/null
  chk "toggle: file is plain on disk to start with" "$(grep -c convert-marker-AAA "$T/store/users/admin/cv.txt")" 1
  chk "toggle: short passphrase refused" "$(code -b $J -X POST -d '{"mode":"encrypt","passphrase":"short"}' $B/api/vault/convert)" 400
  chk "toggle: login password refused as passphrase" "$(code -b $J -X POST -d '{"mode":"encrypt","passphrase":"password123"}' $B/api/vault/convert)" 400
  R=$($C -X POST -d "{\"mode\":\"encrypt\",\"passphrase\":\"$PASS_PHRASE\"}" $B/api/vault/convert)
  chk "toggle: turning encryption on returns a recovery key" "$(echo "$R" | grep -c '"recovery_key":"')" 1
  waitconv
  chk "toggle: existing file is now encrypted on disk" "$(grep -c convert-marker-AAA "$T/store/users/admin/cv.txt")" 0
  chk "toggle: no plaintext left anywhere" "$(grep -rl convert-marker-AAA "$T/store/users" | wc -l)" 0
  chk "toggle: app still opens the file (no lock screen)" "$($C "$B/api/download?path=/cv.txt")" convert-marker-AAA
  chk "toggle: size shown is the real size" "$($C "$B/api/list?path=/" | python3 -c "import sys,json;print([i['size'] for i in json.load(sys.stdin)['items'] if i['name']=='cv.txt'][0])")" 19
  chk "toggle: status says encrypted and unlocked" "$(curl -s $B/api/status | grep -c '"encrypted":true,')$(curl -s $B/api/status | grep -c '"locked":false')" 11
  echo "new-after-on-BBB" > "$T/cv2.txt"; $C -X PUT --data-binary @$T/cv2.txt "$B/api/upload?path=/&name=cv2.txt" >/dev/null
  chk "toggle: new upload is encrypted" "$(grep -c new-after-on-BBB "$T/store/users/admin/cv2.txt")" 0
  chk "toggle: wrong passphrase cannot turn it off" "$(code -b $J -X POST -d '{"mode":"decrypt","passphrase":"not the passphrase"}' $B/api/vault/convert)" 401
  chk "toggle: turning encryption off" "$(code -b $J -X POST -d "{\"mode\":\"decrypt\",\"passphrase\":\"$PASS_PHRASE\"}" $B/api/vault/convert)" 200
  waitconv
  chk "toggle: files are plain on disk again" "$(grep -c convert-marker-AAA "$T/store/users/admin/cv.txt")$(grep -c new-after-on-BBB "$T/store/users/admin/cv2.txt")" 11
  chk "toggle: key file removed" "$([ -e "$T/store/.nestnas-vault" ] && echo present || echo gone)" gone
  chk "toggle: status says not encrypted" "$(curl -s $B/api/status | grep -c '"encrypted":true')" 0
  chk "toggle: app still opens the file" "$($C "$B/api/download?path=/cv.txt")" convert-marker-AAA
  chk "toggle: key-less upload is plain again" "$(echo plain-again-CCC > "$T/c3.txt"; $C -X PUT --data-binary @$T/c3.txt "$B/api/upload?path=/&name=c3.txt" >/dev/null; grep -c plain-again-CCC "$T/store/users/admin/c3.txt")" 1
fi
exit $fail
