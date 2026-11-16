set -euo pipefail
shell() { mongosh --quiet --port 57017 --eval "$1"; }
ready() { [ "$(shell 'db.runCommand({ ping: 1 }).ok' 2> /dev/null)" = 1 ]; }
for _ in $(seq 60); do ready && break; sleep 1; done
ready
[ "$(shell 'db.version()')" = "$DATABASIFY_RELEASE" ]
[ "$(shell 'db.serverStatus().tcmalloc.usingPerCPUCaches ?? false')" = false ]
shell 'rs.initiate({ _id: "smoke", members: [{ _id: 0, host: "127.0.0.1:57017" }] }).ok' > /dev/null
primary() { [ "$(shell 'db.hello().isWritablePrimary')" = true ]; }
for _ in $(seq 60); do primary && break; sleep 1; done
primary
[ "$(shell 'db.getSiblingDB("smoke").checks.insertOne({ ok: 1 }).acknowledged')" = true ]
databasify ctl is-active --quiet server.service
[ "$(databasify ctl show -p NRestarts --value server)" = 0 ]
databasify ctl logs -u server -n 3
pbm version --short
pbm-agent version --short
pcsm version
mongodump --version | sed -n 1p
mongorestore --version | sed -n 1p
echo "mongodb $DATABASIFY_RELEASE runs under the databasify supervisor as a one-member replica set"
[ "$(id -u mongodb)" = 64999 ] && [ "$(id -g mongodb)" = 64999 ]
