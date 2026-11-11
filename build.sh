#!/usr/bin/env bash
# Builds the provider into its Pome (pome/provider.wasm) and writes its sha256 into
# pome/mamela-provider.toml. The build is reproducible: TestPomeProgramIsTheBuild rebuilds it and
# checks that the committed program is exactly this build of the source.
set -euo pipefail
cd "$(dirname "$0")"
# The same source and the same Go give the same bytes (CI checks it): build with exactly this Go.
want_go=go1.27.3
got_go="$(GOTOOLCHAIN=${GOTOOLCHAIN:-auto} go env GOVERSION)"
if [[ "$got_go" != "$want_go" ]]; then
  echo "build with $want_go (this is $got_go): GOTOOLCHAIN=$want_go ./build.sh" >&2
  exit 1
fi
# The catalogs of the APIs (ADR 0008 of Mamela), as data in the Pome.
mkdir -p pome/catalogs
GOWORK=off go run ./cmd/aws-catalogs pome/catalogs
out=pome/provider.wasm
GOOS=wasip1 GOARCH=wasm GOWORK=off go build -trimpath -buildvcs=false -o "$out" ./cmd/aws-provider
sum="$(shasum -a 256 "$out" | cut -d' ' -f1)"
version="$(sed -n 's/^\tVersion = "\(.*\)"$/\1/p' aws/aws.go)"
cat > pome/mamela-provider.toml <<TOML
# The provider program of this Pome. Mamela runs it isolated (WASM) and checks this sha256;
# Pome.lock and the sumdb pin the whole Pome, this file and the program included.
provider = "aws"
version = "$version"
program = "provider.wasm"
sha256 = "$sum"

# The catalogs of the APIs the provider calls (ADR 0008 of Mamela). Mamela classifies the program's
# requests with one only when the operator names it (mamela.providers.toml: catalog = "pome:ec2").
[catalogs]
ec2 = "catalogs/ec2.json"
ecs = "catalogs/ecs.json"
iam = "catalogs/iam.json"
logs = "catalogs/logs.json"
ssm = "catalogs/ssm.json"
TOML
echo "$out $sum (version $version)"
