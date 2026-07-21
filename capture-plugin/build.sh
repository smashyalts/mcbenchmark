#!/usr/bin/env bash
# Builds BenchCapture-1.1.0-beta.jar using only a JDK (>=21) + curl — no Maven needed.
# It fetches the paper-api compile dependencies into libs/ (cached) and packages
# the plugin jar. For a Maven build instead, run: mvn -q package
set -euo pipefail
cd "$(dirname "$0")"

PAPER_V="1.21.4-R0.1-20250925.065901-231"
# Compile against the OLDEST PacketEvents we must run on, not the newest that
# exists. Production currently ships 2.12.1; building against 2.13.0 and
# deploying onto 2.12.1 compiles cleanly and then throws NoSuchMethodError at
# runtime, on the packet path, under load. Raise this only together with the
# server's own PacketEvents.
PE_V="2.12.1"
PAPER_BASE="https://repo.papermc.io/repository/maven-public/io/papermc/paper/paper-api/1.21.4-R0.1-SNAPSHOT"
MC="https://repo1.maven.org/maven2"

mkdir -p libs
fetch() { # url dest
  if [ ! -f "$2" ]; then echo "fetch $(basename "$2")"; curl -fsSL "$1" -o "$2"; fi
}
fetch "$PAPER_BASE/paper-api-$PAPER_V.jar"                                   libs/paper-api.jar
fetch "$MC/org/jetbrains/annotations/24.1.0/annotations-24.1.0.jar"         libs/annotations.jar
fetch "$MC/net/kyori/adventure-api/4.17.0/adventure-api-4.17.0.jar"         libs/adventure-api.jar
fetch "$MC/net/kyori/adventure-key/4.17.0/adventure-key-4.17.0.jar"         libs/adventure-key.jar
fetch "$MC/net/kyori/examination-api/1.3.0/examination-api-1.3.0.jar"       libs/examination-api.jar
fetch "$MC/com/google/guava/guava/33.3.1-jre/guava-33.3.1-jre.jar"          libs/guava.jar
# Movement capture reads serverbound packets, which needs the PacketEvents API
# at compile time (and the plugin at runtime — see plugin.yml depend).
fetch "https://github.com/retrooper/packetevents/releases/download/v${PE_V}/packetevents-spigot-${PE_V}.jar" libs/packetevents.jar

# Windows JDKs use ';' as the classpath separator; POSIX JDKs use ':'.
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) SEP=';' ;;
  *) SEP=':' ;;
esac
CP=$(printf "libs/%s${SEP}" paper-api.jar annotations.jar adventure-api.jar adventure-key.jar examination-api.jar guava.jar packetevents.jar)

rm -rf out && mkdir -p out
javac --release 21 -cp "$CP" -d out $(find src/main/java -name '*.java')
cp src/main/resources/plugin.yml src/main/resources/config.yml out/
rm -f BenchCapture-1.1.0-beta.jar
jar --create --file BenchCapture-1.1.0-beta.jar -C out .

# Ring and spatial-index checks. These cover the lock-free handoff and the
# packed slot layout, where a mistake corrupts capture data silently rather
# than throwing, so they run on every build.
javac --release 21 -cp "${CP}out" -d out tools/RingTest.java
java -cp "${CP}out" RingTest

rm -rf out
echo "built BenchCapture-1.1.0-beta.jar"
