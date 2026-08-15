#!/usr/bin/env bash
#
# generate.sh — regenerate the Go protobuf/gRPC bindings under gen/ from the
# vendored NPLN protocol definitions under protocol/.
#
# The generated code IS COMMITTED to the repository, so building the server only
# needs a Go toolchain. You only need to run this script if you change or update
# something under protocol/.
#
# Requirements:
#   protoc              >= 25   (https://github.com/protocolbuffers/protobuf/releases)
#   protoc-gen-go       go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6
#   protoc-gen-go-grpc  go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
#
# Why the M<file>=<import path> flags below?
#   Nintendo's protocol files carry either no `go_package` option at all (the
#   Splatoon 3 "toyohr" ones) or one pointing at Nintendo's internal module path
#   ("npln.nintendo.net/npln-practice/..."). Rather than editing the vendored
#   files — which would make it impossible to diff them against upstream when
#   kinnay's decompilation is updated — we map every file to a package inside
#   THIS module here, at generation time. The vendored .proto tree stays a
#   byte-for-byte copy of upstream.
set -euo pipefail

cd "$(dirname "$0")/.."

MODULE="github.com/NextendoNetwork/splatoon-3"
OUT="gen"
INCLUDE="protocol"

# Every NPLN protocol file we compile, and the Go import path it lands in.
# Keep this list in sync with the files under protocol/proto.
declare -a MAPPINGS=(
  "proto/appconfig/v1/appconfig.proto=$MODULE/$OUT/npln/appconfig/v1;appconfigv1"
  "proto/auth/v1/auth.proto=$MODULE/$OUT/npln/auth/v1;authv1"
  "proto/auth/v1/resources.proto=$MODULE/$OUT/npln/auth/v1;authv1"
  "proto/auth/v1/user.proto=$MODULE/$OUT/npln/auth/v1;authv1"
  "proto/common/heartbeat.proto=$MODULE/$OUT/npln/common;commonpb"
  "proto/common/resources.proto=$MODULE/$OUT/npln/common;commonpb"
  "proto/common/value.proto=$MODULE/$OUT/npln/common;commonpb"
  "proto/errdetails/nerror.proto=$MODULE/$OUT/npln/errdetails;errdetails"
  "proto/friends/v1/friends.proto=$MODULE/$OUT/npln/friends/v1;friendsv1"
  "proto/friends/v1/presence.proto=$MODULE/$OUT/npln/friends/v1;friendsv1"
  "proto/friends/v1/resources.proto=$MODULE/$OUT/npln/friends/v1;friendsv1"
  "proto/gamesync/v1/gamesync.proto=$MODULE/$OUT/npln/gamesync/v1;gamesyncv1"
  "proto/gamesync/v1/resources.proto=$MODULE/$OUT/npln/gamesync/v1;gamesyncv1"
  "proto/globalcounter/v1/global_counter.proto=$MODULE/$OUT/npln/globalcounter/v1;globalcounterv1"
  "proto/globalcounter/v1/resources.proto=$MODULE/$OUT/npln/globalcounter/v1;globalcounterv1"
  "proto/hydro/v1/datastore.proto=$MODULE/$OUT/npln/hydro/v1;hydrov1"
  "proto/hydro/v1/resources.proto=$MODULE/$OUT/npln/hydro/v1;hydrov1"
  "proto/leaderboard/v1/leaderboard.proto=$MODULE/$OUT/npln/leaderboard/v1;leaderboardv1"
  "proto/leaderboard/v1/resources.proto=$MODULE/$OUT/npln/leaderboard/v1;leaderboardv1"
  "proto/maintenance/v1/maintenance_schedule.proto=$MODULE/$OUT/npln/maintenance/v1;maintenancev1"
  "proto/maintenance/v1/resources.proto=$MODULE/$OUT/npln/maintenance/v1;maintenancev1"
  "proto/matchmaking/v1/game_session_service.proto=$MODULE/$OUT/npln/matchmaking/v1;matchmakingv1"
  "proto/matchmaking/v1/matchmaker.proto=$MODULE/$OUT/npln/matchmaking/v1;matchmakingv1"
  "proto/matchmaking/v1/resources.proto=$MODULE/$OUT/npln/matchmaking/v1;matchmakingv1"
  "proto/messaging/v1/messaging.proto=$MODULE/$OUT/npln/messaging/v1;messagingv1"
  "proto/toyohr/v1/canola.proto=$MODULE/$OUT/npln/toyohr/v1;toyohrv1"
  "proto/toyohr/v1/cloud_save.proto=$MODULE/$OUT/npln/toyohr/v1;toyohrv1"
  "proto/toyohr/v1/common.proto=$MODULE/$OUT/npln/toyohr/v1;toyohrv1"
  "proto/toyohr/v1/coop_scenario.proto=$MODULE/$OUT/npln/toyohr/v1;toyohrv1"
  "proto/toyohr/v1/fest.proto=$MODULE/$OUT/npln/toyohr/v1;toyohrv1"
  "proto/toyohr/v1/game_record.proto=$MODULE/$OUT/npln/toyohr/v1;toyohrv1"
  "proto/toyohr/v1/lobby_messaging.proto=$MODULE/$OUT/npln/toyohr/v1;toyohrv1"
  "proto/toyohr/v1/locker.proto=$MODULE/$OUT/npln/toyohr/v1;toyohrv1"
  "proto/toyohr/v1/replay.proto=$MODULE/$OUT/npln/toyohr/v1;toyohrv1"
  "proto/toyohr/v1/resources.proto=$MODULE/$OUT/npln/toyohr/v1;toyohrv1"
  "proto/toyohr/v1/schedule.proto=$MODULE/$OUT/npln/toyohr/v1;toyohrv1"
  "proto/toyohr/v1/userscreening.proto=$MODULE/$OUT/npln/toyohr/v1;toyohrv1"
  "proto/ugcstore/v1/query.proto=$MODULE/$OUT/npln/ugcstore/v1;ugcstorev1"
  "proto/ugcstore/v1/resources.proto=$MODULE/$OUT/npln/ugcstore/v1;ugcstorev1"
  "proto/ugcstore/v1/screening.proto=$MODULE/$OUT/npln/ugcstore/v1;ugcstorev1"
  "proto/ugcstore/v1/ugcstore.proto=$MODULE/$OUT/npln/ugcstore/v1;ugcstorev1"
  "proto/ugcstore/v1/write.proto=$MODULE/$OUT/npln/ugcstore/v1;ugcstorev1"
  # Third-party support files the NPLN definitions import. Nintendo's copies of
  # google/api and validate are compiled into this module too, so the server has
  # no dependency on any external protobuf registry.
  "google/api/annotations.proto=$MODULE/$OUT/third_party/googleapi;googleapi"
  "google/api/client.proto=$MODULE/$OUT/third_party/googleapi;googleapi"
  "google/api/field_behavior.proto=$MODULE/$OUT/third_party/googleapi;googleapi"
  "google/api/http.proto=$MODULE/$OUT/third_party/googleapi;googleapi"
  "google/api/resource.proto=$MODULE/$OUT/third_party/googleapi;googleapi"
  "google/rpc/status.proto=$MODULE/$OUT/third_party/googlerpc;googlerpc"
  "validate/validate.proto=$MODULE/$OUT/third_party/validate;validatepb"
)

OPTS=""
for m in "${MAPPINGS[@]}"; do
  OPTS="$OPTS --go_opt=M$m --go-grpc_opt=M$m"
done

rm -rf "$OUT"
mkdir -p "$OUT"

# shellcheck disable=SC2086
protoc \
  -I "$INCLUDE" \
  --go_out=. --go_opt=module="$MODULE" \
  --go-grpc_out=. --go-grpc_opt=module="$MODULE" --go-grpc_opt=require_unimplemented_servers=false \
  $OPTS \
  $(cd "$INCLUDE" && find proto google validate -name '*.proto' | sort)

echo "generated:"
find "$OUT" -name '*.go' | sort
