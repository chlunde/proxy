#!/bin/bash
# Manual tests for "Send Maven publish time on .pom responses".
# usage: manual.sh [IMAGE] (default proxy-e2e:b; build: docker build -t proxy-e2e:b .)
# Needs docker, curl, and the dependabot CLI on PATH or in $DEPENDABOT.
set -u
cd "$(dirname "$0")"
export IMAGE=${1:-proxy-e2e:b}
DBOT=${DEPENDABOT:-dependabot}
P=http://localhost:18090/maven
G=com/google/guava/guava
POM=$G/33.4.0-jre/guava-33.4.0-jre.pom
JAR=$G/33.4.0-jre/guava-33.4.0-jre.jar
fail=0
ok()  { printf 'PASS %s\n' "$1"; }
bad() { printf 'FAIL %s: %s\n' "$1" "$2"; fail=1; }
lm()  { curl -s -o /dev/null -D - -m 20 "$@" | tr -d '\r' | sed -n 's/^[Ll]ast-[Mm]odified: //p'; }
code(){ curl -s -o /dev/null -w '%{http_code}' -m 20 "$@"; }
ups() { grep -c '"event":"upstream"' logs/access.jsonl 2>/dev/null || echo 0; }
up()  { docker compose up -d --wait proxy upstream >/dev/null 2>&1 || docker compose up -d >/dev/null; sleep 2; }

docker compose down -v >/dev/null 2>&1; rm -rf logs; mkdir -p logs; up

# M1 cold HEAD on a release .pom carries upstream's Last-Modified.
want=$(lm -I https://maven-central.storage-download.googleapis.com/maven2/$POM)
got=$(lm -I $P/gcs/$POM)
[ -n "$got" ] && [ "$got" = "$want" ] && ok "M1 cold HEAD pom: $got" || bad M1 "got '$got' want '$want'"

# M2 cached GET and HEAD give the same date and no upstream call.
n=$(ups); g=$(lm $P/gcs/$POM); h=$(lm -I $P/gcs/$POM); sleep 1
[ "$g" = "$want" ] && [ "$h" = "$want" ] && [ "$(ups)" = "$n" ] && ok "M2 cached GET/HEAD, 0 upstream" || bad M2 "GET '$g' HEAD '$h' upstream $n→$(ups)"

# M3 the jar and the .pom.sha1 carry no date.
j=$(lm -I $P/gcs/$JAR); s=$(lm -I $P/gcs/$POM.sha1)
[ -z "$j" ] && [ -z "$s" ] && ok "M3 jar/sha1 undated" || bad M3 "jar '$j' sha1 '$s'"

# M4 the date survives a restart (it is in the database).
docker compose restart proxy >/dev/null; sleep 3
r=$(lm -I $P/gcs/$POM)
[ "$r" = "$want" ] && ok "M4 after restart" || bad M4 "got '$r'"

# M5 upstream without Last-Modified: none sent, never the fetch time.
a=$(lm -I $P/nolm/com/example/nolm/1.0/nolm-1.0.pom); b=$(lm $P/nolm/com/example/nolm/1.0/nolm-1.0.pom)
[ -z "$a$b" ] && ok "M5 no invented date" || bad M5 "HEAD '$a' GET '$b'"

# M6 hanging upstream HEAD: the cached .pom answers in ~5s, undated.
curl -s -o /dev/null -m 20 $P/nolm/com/example/slowhead/1.0/slowhead-1.0.pom
t0=$(date +%s); c=$(code -I $P/nolm/com/example/slowhead/1.0/slowhead-1.0.pom); dt=$(( $(date +%s) - t0 ))
[ "$c" = 200 ] && [ $dt -le 8 ] && ok "M6 slow HEAD bounded: ${dt}s" || bad M6 "status $c after ${dt}s"

# M7 upstream HEAD 503: cached .pom still 200.
curl -s -o /dev/null -m 20 $P/nolm/com/example/headfail/1.0/headfail-1.0.pom
c=$(code -I $P/nolm/com/example/headfail/1.0/headfail-1.0.pom)
[ "$c" = 200 ] && ok "M7 HEAD 503 upstream → 200" || bad M7 "status $c"

# M8 Dependabot, cooldown 30d, via the undated GCS mirror: the newest
# guava is filtered, and no "cooldown date unavailable".
cat > job.yaml <<J
job:
  package-manager: maven
  allowed-updates: [{update-type: all}]
  source: {provider: github, repo: example/cooldown, directory: /}
  cooldown: {default-days: 30}
credentials:
  - {type: maven_repository, url: "http://host.docker.internal:18090/maven/gcs", replaces-base: true}
J
for run in cold warm; do
  "$DBOT" update -f job.yaml --local ./repo -o out-$run.yaml > logs/dependabot-$run.txt 2>&1
  f=$(grep -c 'Filtered out (cooldown)' logs/dependabot-$run.txt); u=$(grep -c cooldown_date_unavailable logs/dependabot-$run.txt)
  to=$(grep -oE 'Updating com.google.guava:guava from [^ ]+ to [^ ]+' logs/dependabot-$run.txt | head -1)
  [ "$f" -gt 0 ] && [ "$u" = 0 ] && ok "M8 dependabot $run: filtered=$f, $to" || bad "M8 $run" "filtered=$f unavailable=$u ($to)"
done

# M9 upgrade from main on the same volume: a .pom cached by main gets
# its date on the first HEAD. Needs IMAGE_BASE (default proxy-e2e:base).
docker compose down -v >/dev/null 2>&1
IMAGE=${IMAGE_BASE:-proxy-e2e:base} up
curl -s -o /dev/null -m 20 $P/gcs/$POM
d=$(lm -I $P/gcs/$POM)
docker compose up -d >/dev/null; sleep 3
u=$(lm -I $P/gcs/$POM)
[ -z "$d" ] && [ "$u" = "$want" ] && ok "M9 upgrade backfill: main '' → $u" || bad M9 "main '$d' new '$u'"

docker compose down -v >/dev/null 2>&1
exit $fail
